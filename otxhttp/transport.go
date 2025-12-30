package otxhttp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type TransportLogger struct {
	base http.RoundTripper
}

func (t TransportLogger) RoundTrip(r *http.Request) (res *http.Response, err error) {
	ctx := context.WithoutCancel(r.Context())

	attrs := []any{
		slog.String(string(semconv.HTTPRequestMethodKey), r.Method),
		slog.String(string(semconv.URLOriginalKey), r.URL.String()),
	}

	l := log.From(ctx)
	l.Info("HTTP req", attrs...)

	t0 := time.Now()
	res, err = t.base.RoundTrip(r)
	dt := time.Since(t0)

	if err != nil {
		l.Warn("HTTP err: "+err.Error(), append(attrs,
			slog.Int("client.elapsed_ns", int(dt.Nanoseconds())),
		)...)
		return
	}

	level := slog.LevelInfo
	if res.StatusCode >= 400 {
		level = slog.LevelWarn
	}

	attrs = append(attrs,
		slog.Int(string(semconv.HTTPResponseStatusCodeKey), res.StatusCode),
	)
	attrs_ := attrs
	if h := res.Header.Get("Content-Length"); h != "" {
		attrs_ = append(attrs_, slog.String("http.response.header.content-length", h))
	}

	l.Log(ctx, level, "HTTP res", append(attrs_,
		slog.Int(string(semconv.HTTPResponseStatusCodeKey), res.StatusCode),
		slog.Int("client.elapsed_ns", int(dt.Nanoseconds())),
	)...)

	res.Body = &httpResBody{
		ReadCloser: res.Body,

		log:   l,
		t0:    t0,
		attrs: attrs,
	}
	return res, err
}

func NewTransport(base http.RoundTripper, opts ...otelhttp.Option) http.RoundTripper {
	return otelhttp.NewTransport(TransportLogger{base}, opts...)
}

type httpResBody struct {
	io.ReadCloser

	log   *slog.Logger
	t0    time.Time
	attrs []any

	size atomic.Int64
	done atomic.Bool
}

func (r *httpResBody) Read(b []byte) (n int, err error) {
	n, err = r.ReadCloser.Read(b)
	r.size.Add(int64(n))

	switch err {
	case nil:
	case io.EOF:
		r.finalize()

	default:
	}

	return
}

func (r *httpResBody) Close() error {
	err := r.ReadCloser.Close()
	r.finalize()
	return err
}

func (r *httpResBody) finalize() {
	if r.done.Swap(true) {
		return
	}

	dt := time.Since(r.t0)
	r.log.Info("HTTP end", append(r.attrs,
		slog.Int(string(semconv.HTTPResponseBodySizeKey), int(r.size.Load())),
		slog.Int("client.elapsed_ns", int(dt.Nanoseconds())),
	)...)
}
