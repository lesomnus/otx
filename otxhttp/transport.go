package otxhttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// NewTransport returns a [net/http.RoundTripper] that traces each request with
// otelhttp and logs it.
//
// base defaults to [net/http.DefaultTransport]. The tracer provider, meter
// provider and propagator of x are given to otelhttp; anything in opts is
// applied after them and so wins. x is also put into the request context, so
// the records reach the logger provider it holds even for a request built from
// a bare [context.Background].
//
// Three records are written per request: "HTTP req" when it is sent, "HTTP
// res" when the response head arrives, and "HTTP end" when the response body
// is exhausted or closed. A body that is neither read to the end nor closed
// produces no "HTTP end"; that is a leak in the calling code, and it is the
// same condition under which otelhttp never ends its span.
//
// It panics if x is nil, at wiring time rather than on the first request.
func NewTransport(x *otx.Otx, base http.RoundTripper, opts ...otelhttp.Option) http.RoundTripper {
	if x == nil {
		panic("otxhttp: NewTransport called with a nil *otx.Otx")
	}
	if base == nil {
		base = http.DefaultTransport
	}

	ps := x.Providers()
	opts = append([]otelhttp.Option{
		otelhttp.WithTracerProvider(ps.Tracer()),
		otelhttp.WithMeterProvider(ps.Meter()),
		otelhttp.WithPropagators(x.Propagator()),
	}, opts...)

	return otelhttp.NewTransport(&TransportLogger{otx: x, base: base}, opts...)
}

// TransportLogger logs each request that passes through it. It is the logging
// half of [NewTransport], exposed so that it can be composed with an otelhttp
// setup of your own; note that it must sit *inside*
// [otelhttp.NewTransport] for its records to carry the client span ids.
type TransportLogger struct {
	otx  *otx.Otx
	base http.RoundTripper
}

var _ http.RoundTripper = (*TransportLogger)(nil)

// NewTransportLogger returns a [TransportLogger] over base, which defaults to
// [net/http.DefaultTransport].
func NewTransportLogger(x *otx.Otx, base http.RoundTripper) *TransportLogger {
	if x == nil {
		panic("otxhttp: NewTransportLogger called with a nil *otx.Otx")
	}
	if base == nil {
		base = http.DefaultTransport
	}

	return &TransportLogger{otx: x, base: base}
}

func (t *TransportLogger) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.otx != nil {
		r = r.WithContext(otx.Into(r.Context(), t.otx))
	}

	// Detached from cancellation so that the records of a request that is
	// cancelled mid-flight still reach the exporter.
	ctx := context.WithoutCancel(r.Context())

	attrs := []any{
		slog.String(string(semconv.HTTPRequestMethodKey), r.Method),
		slog.String(string(semconv.URLFullKey), urlFull(r.URL)),
	}

	l := log.From(ctx)
	l.Info("HTTP req", attrs...)

	t0 := time.Now()
	res, err := t.base.RoundTrip(r)
	dt := time.Since(t0)

	if err != nil {
		// The error text stays in an attribute: putting it in the message
		// would make every distinct host, port and timeout its own message.
		l.Log(ctx, slog.LevelWarn, "HTTP err", append(attrs,
			slog.String(log.ErrorTypeKey, log.ErrorType(err)),
			slog.String(log.ErrorMessageKey, err.Error()),
			slog.Int64("client.elapsed_ns", dt.Nanoseconds()),
		)...)
		return nil, err
	}

	level := slog.LevelInfo
	if res.StatusCode >= 400 {
		level = slog.LevelWarn
	}

	attrs = append(attrs, slog.Int(string(semconv.HTTPResponseStatusCodeKey), res.StatusCode))

	res_attrs := append(attrs[:len(attrs):len(attrs)],
		slog.Int64("client.elapsed_ns", dt.Nanoseconds()),
	)
	if v := res.Header.Get("Content-Length"); v != "" {
		res_attrs = append(res_attrs, slog.String("http.response.header.content-length", v))
	}
	l.Log(ctx, level, "HTTP res", res_attrs...)

	res.Body = wrapResBody(res.Body, l, t0, attrs)
	return res, nil
}

// urlFull renders the URL for the url.full attribute with any userinfo
// removed, as the semantic convention requires and as otelhttp does for the
// span attribute of the same request. Secrets in the query string are recorded
// as they are.
func urlFull(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.User == nil {
		return u.String()
	}

	v := *u
	v.User = nil

	return v.String()
}

// wrapResBody wraps body so that its size and lifetime can be recorded,
// leaving it alone when there is nothing to measure and preserving the writer
// of a 101 Switching Protocols body, which the caller needs to speak the
// upgraded protocol.
func wrapResBody(body io.ReadCloser, l *slog.Logger, t0 time.Time, attrs []any) io.ReadCloser {
	if body == nil || body == http.NoBody {
		return body
	}

	v := &httpResBody{
		ReadCloser: body,

		log:   l,
		t0:    t0,
		attrs: attrs,
	}
	if rw, ok := body.(io.ReadWriteCloser); ok {
		return httpResBodyRW{httpResBody: v, writer: rw}
	}

	return v
}

type httpResBody struct {
	io.ReadCloser

	log   *slog.Logger
	t0    time.Time
	attrs []any

	size atomic.Int64
	done atomic.Bool
}

func (r *httpResBody) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.size.Add(int64(n))
	if errors.Is(err, io.EOF) {
		r.finalize()
	}

	return n, err
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
	r.log.Info("HTTP end", append(r.attrs[:len(r.attrs):len(r.attrs)],
		slog.Int64(string(semconv.HTTPResponseBodySizeKey), r.size.Load()),
		slog.Int64("client.elapsed_ns", dt.Nanoseconds()),
	)...)
}

// httpResBodyRW keeps a body that is also an [io.Writer] writable. net/http
// hands back an [io.ReadWriteCloser] for a 101 response, and a reverse proxy
// type-asserts it to speak the upgraded protocol.
type httpResBodyRW struct {
	*httpResBody
	writer io.Writer
}

var _ io.ReadWriteCloser = httpResBodyRW{}

func (r httpResBodyRW) Write(b []byte) (int, error) {
	return r.writer.Write(b)
}
