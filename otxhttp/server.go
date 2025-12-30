package otxhttp

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/felixge/httpsnoop"
	"github.com/lesomnus/otx/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
)

func BoundaryLogger() func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithoutCancel(r.Context())

			peer_addr := r.RemoteAddr
			if i := strings.LastIndexByte(peer_addr, ':'); i > 0 {
				peer_addr = peer_addr[:i]
			}

			l := log.From(ctx)
			l.Info("HTTP in",
				slog.String(string(semconv.HTTPRequestMethodKey), r.Method),
				slog.String(string(semconv.URLPathKey), r.URL.Path),
				slog.String(string(semconv.NetworkPeerAddressKey), peer_addr),
			)

			m := httpsnoop.CaptureMetrics(h, w, r)

			level := slog.LevelInfo
			if m.Code >= 500 {
				level = slog.LevelError
			} else if m.Code >= 400 {
				level = slog.LevelWarn
			}
			l.Log(ctx, level, "HTTP out",
				slog.String(string(semconv.HTTPRequestMethodKey), r.Method),
				slog.String(string(semconv.URLPathKey), r.URL.Path),
				slog.Int(string(semconv.HTTPResponseStatusCodeKey), m.Code),
				slog.Int(string(semconv.HTTPResponseBodySizeKey), int(m.Written)),
				slog.Int64("server.elapsed_ns", m.Duration.Nanoseconds()),
			)
		})
	}
}
