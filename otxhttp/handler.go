// Package otxhttp carries an [github.com/lesomnus/otx.Otx] across an HTTP
// boundary, on the server side as middleware and on the client side as a
// [net/http.RoundTripper].
//
// Both wrap the corresponding
// [go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp]
// instrumentation with the providers and propagator held by the Otx, and put
// the Otx into the request context so that
// [github.com/lesomnus/otx/log.From] resolves further down.
//
// A server usually mounts both middlewares, the boundary logger innermost so
// that its records carry the ids of the span the outer middleware started:
//
//	h = otxhttp.NewMiddleware(x, "api")(otxhttp.BoundaryLogger(x)(h))
package otxhttp

import (
	"net/http"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// NewHandler wraps handler with [NewMiddleware].
func NewHandler(x *otx.Otx, handler http.Handler, operation string, opts ...otelhttp.Option) http.Handler {
	return NewMiddleware(x, operation, opts...)(handler)
}

// NewMiddleware returns a middleware that starts a server span for each
// request and puts x into the request context.
//
// The tracer provider, meter provider and propagator of x are given to
// otelhttp. The server span keeps op as its name; anything in opts is applied
// after these defaults and so wins, including a caller's span name formatter.
//
// It panics if x is nil, at wiring time rather than on the first request.
func NewMiddleware(x *otx.Otx, op string, opts ...otelhttp.Option) func(http.Handler) http.Handler {
	if x == nil {
		panic("otxhttp: NewMiddleware called with a nil *otx.Otx")
	}

	ps := x.Providers()
	opts = append([]otelhttp.Option{
		otelhttp.WithTracerProvider(ps.Tracer()),
		otelhttp.WithMeterProvider(ps.Meter()),
		otelhttp.WithPropagators(x.Propagator()),
		// otelhttp's default now names the span after the HTTP method and
		// route. Keep the operation this API was handed, as before.
		otelhttp.WithSpanNameFormatter(func(operation string, _ *http.Request) string {
			return operation
		}),
	}, opts...)

	mw := otelhttp.NewMiddleware(op, opts...)
	return func(h http.Handler) http.Handler {
		next := mw(h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otx.Into(r.Context(), x)
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)
		})
	}
}
