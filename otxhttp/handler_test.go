package otxhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxhttp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNewMiddleware(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxhttp: NewMiddleware called with a nil *otx.Otx", func() {
			otxhttp.NewMiddleware(nil, "api")
		})
	})

	t.Run("puts the otx into the request context", func(t *testing.T) {
		h := newHarness(t)

		var (
			given *otx.Otx
			ok    bool
		)
		handler := otxhttp.NewMiddleware(h.otx, "api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			given, ok = otx.FromOK(r.Context())
		}))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.True(t, ok, "the handler must be able to read the Otx back out")
		require.Same(t, h.otx, given)
	})

	t.Run("starts a server span", func(t *testing.T) {
		h := newHarness(t)

		var span_ctx trace.SpanContext
		handler := otxhttp.NewMiddleware(h.otx, "api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			span_ctx = trace.SpanContextFromContext(r.Context())
			w.WriteHeader(http.StatusTeapot)
		}))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.True(t, span_ctx.IsValid(), "the handler must run inside the server span")

		spans := h.spans.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, "api", spans[0].Name())
		require.Equal(t, trace.SpanKindServer, spans[0].SpanKind())
		require.Equal(t, span_ctx.SpanID(), spans[0].SpanContext().SpanID())
	})

	t.Run("the handler logs into the logger provider of the otx", func(t *testing.T) {
		h := newHarness(t)

		var span_ctx trace.SpanContext
		handler := otxhttp.NewMiddleware(h.otx, "api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			span_ctx = trace.SpanContextFromContext(r.Context())
			log.From(r.Context()).Info("in the handler")
		}))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.Equal(t, []string{"in the handler"}, h.messages())
		requireSpanIDs(t, h.record(t, "in the handler"), span_ctx)
	})

	t.Run("the propagator of the otx joins the remote trace", func(t *testing.T) {
		h := newHarness(t)

		trace_id, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
		require.NoError(t, err)
		span_id, err := trace.SpanIDFromHex("00f067aa0ba902b7")
		require.NoError(t, err)

		handler := otxhttp.NewMiddleware(h.otx, "api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil)
		r.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		spans := h.spans.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, trace_id, spans[0].Parent().TraceID())
		require.Equal(t, span_id, spans[0].Parent().SpanID())
		require.Equal(t, trace_id, spans[0].SpanContext().TraceID())
	})

	t.Run("options given by the caller win over the defaults", func(t *testing.T) {
		h := newHarness(t)

		// The overlapping option is the one that decides this: an option that
		// the defaults do not set, such as the span name formatter, would take
		// effect whichever way round the two slices were joined. The tracer
		// provider is set by both, so the spans land in exactly one of the two
		// recorders and that says which slice was applied last.
		other := tracetest.NewSpanRecorder()
		other_provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(other))
		t.Cleanup(func() {
			require.NoError(t, other_provider.Shutdown(context.Background()))
		})

		handler := otxhttp.NewMiddleware(h.otx, "api",
			otelhttp.WithTracerProvider(other_provider),
			otelhttp.WithSpanNameFormatter(func(op string, r *http.Request) string {
				return op + " " + r.Method
			}),
		)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/v/42", nil))

		require.Empty(t, h.spans.Ended(), "the tracer provider of the otx must lose to the one the caller gave")

		spans := other.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, "api DELETE", spans[0].Name())
	})

	t.Run("the server metrics go to the meter provider of the otx", func(t *testing.T) {
		meter := &meterRecorder{}
		h := newHarness(t, otx.WithMeterProvider(meter))

		const held = 2 * time.Millisecond
		handler := otxhttp.NewMiddleware(h.otx, "api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spin(held)
			w.Write([]byte("hello"))
		}))

		t0 := time.Now()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))
		whole := time.Since(t0)

		// Without the wiring otelhttp would take the global meter provider and
		// this one would only ever see the scope of the Otx itself.
		require.Contains(t, meter.scopeNames(), otelhttp.ScopeName)
		require.Contains(t, meter.instrumentNames(), "http.server.request.duration")
		require.Contains(t, meter.instrumentNames(), "http.server.response.body.size")

		durations := meter.valuesOf("http.server.request.duration")
		require.Len(t, durations, 1, "one request, one duration")
		require.GreaterOrEqual(t, durations[0], held.Seconds(), "the duration is in seconds and covers the handler")
		require.LessOrEqual(t, durations[0], whole.Seconds())

		require.Equal(t, []float64{float64(len("hello"))}, meter.valuesOf("http.server.response.body.size"))
	})
}

func TestNewHandler(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxhttp: NewMiddleware called with a nil *otx.Otx", func() {
			otxhttp.NewHandler(nil, http.NotFoundHandler(), "api")
		})
	})

	t.Run("wraps the handler with the middleware", func(t *testing.T) {
		h := newHarness(t)

		var given *otx.Otx
		handler := otxhttp.NewHandler(h.otx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			given, _ = otx.FromOK(r.Context())
			w.WriteHeader(http.StatusCreated)
		}), "api")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v/42", nil))

		require.Equal(t, http.StatusCreated, w.Code)
		require.Same(t, h.otx, given)

		spans := h.spans.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, "api", spans[0].Name())
	})
}
