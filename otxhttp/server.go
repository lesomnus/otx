package otxhttp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/felixge/httpsnoop"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// BoundaryLogger returns a middleware that writes one record as a request
// enters ("HTTP in") and one as it leaves ("HTTP out"), the latter at Warn for
// a 4xx and at Error for a 5xx or a panic.
//
// It puts x into the request context itself, so it works at any mount point.
// Mounting it inside [NewMiddleware] is still preferable: the server span
// exists by then, so both records carry its ids.
//
// A panic from the wrapped handler is logged and re-raised, leaving the
// recovery to [net/http.Server] as before.
//
// It panics if x is nil, at wiring time rather than on the first request.
func BoundaryLogger(x *otx.Otx) func(http.Handler) http.Handler {
	if x == nil {
		panic("otxhttp: BoundaryLogger called with a nil *otx.Otx")
	}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(otx.Into(r.Context(), x))

			// Detached from cancellation so that the closing record still
			// reaches the exporter when the client hangs up mid-request.
			ctx := context.WithoutCancel(r.Context())

			attrs := []any{
				slog.String(string(semconv.HTTPRequestMethodKey), r.Method),
				slog.String(string(semconv.URLPathKey), r.URL.Path),
			}
			if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				attrs = append(attrs, slog.String(string(semconv.NetworkPeerAddressKey), host))
				if p, err := strconv.Atoi(port); err == nil {
					attrs = append(attrs, slog.Int(string(semconv.NetworkPeerPortKey), p))
				}
			} else if r.RemoteAddr != "" {
				attrs = append(attrs, slog.String(string(semconv.NetworkPeerAddressKey), r.RemoteAddr))
			}

			l := log.From(ctx)
			l.Info("HTTP in", attrs...)

			// Emitted from a defer so that a panicking handler still produces
			// the closing record.
			var m httpsnoop.Metrics
			defer func() {
				v := recover()

				level := slog.LevelInfo
				switch {
				case v != nil, m.Code >= 500:
					level = slog.LevelError
				case m.Code >= 400:
					level = slog.LevelWarn
				}

				out := append(attrs,
					slog.Int(string(semconv.HTTPResponseStatusCodeKey), m.Code),
					slog.Int64(string(semconv.HTTPResponseBodySizeKey), m.Written),
					slog.Int64("server.elapsed_ns", m.Duration.Nanoseconds()),
				)
				if v != nil {
					out = append(out,
						slog.String(log.ErrorTypeKey, fmt.Sprintf("%T", v)),
						slog.String(log.ErrorMessageKey, fmt.Sprint(v)),
					)
				}

				l.Log(ctx, level, "HTTP out", out...)
				if v != nil {
					panic(v)
				}
			}()

			m = httpsnoop.CaptureMetrics(h, w, r)
		})
	}
}
