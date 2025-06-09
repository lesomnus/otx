package otxhttp

import (
	"net/http"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func NewHandler(x *otx.Otx, handler http.Handler, operation string, opts ...otelhttp.Option) http.Handler {
	return NewMiddleware(x, operation, opts...)(handler)
}

func NewMiddleware(x *otx.Otx, op string, opts ...otelhttp.Option) func(http.Handler) http.Handler {
	ps := x.Providers()
	opts = append([]otelhttp.Option{
		otelhttp.WithTracerProvider(ps.Tracer()),
		otelhttp.WithMeterProvider(ps.Meter()),
	}, opts...)

	return func(h http.Handler) http.Handler {
		next := otelhttp.NewMiddleware(op, opts...)(h)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otx.Into(r.Context(), x)
			r = r.WithContext(ctx)

			next.ServeHTTP(w, r)
		})
	}
}
