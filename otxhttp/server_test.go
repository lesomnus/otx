package otxhttp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxhttp"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

func TestBoundaryLogger(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxhttp: BoundaryLogger called with a nil *otx.Otx", func() {
			otxhttp.BoundaryLogger(nil)
		})
	})

	t.Run("writes one record as the request enters and one as it leaves", func(t *testing.T) {
		h := newHarness(t)

		var seen_on_entry int
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen_on_entry = h.logs.Len()
			w.Write([]byte("hello"))
		}))

		r := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/v/42?q=1", nil)
		handler.ServeHTTP(httptest.NewRecorder(), r)

		require.Equal(t, 1, seen_on_entry, `"HTTP in" must be written before the handler runs`)
		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())

		in := h.record(t, "HTTP in")
		require.Equal(t, otellog.SeverityInfo, in.Severity())
		require.Equal(t, http.MethodPatch, attr(t, in, "http.request.method").AsString())
		require.Equal(t, "/v/42", attr(t, in, "url.path").AsString())
		requireNoAttr(t, in, "http.response.status_code")

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityInfo, out.Severity())
		require.Equal(t, http.MethodPatch, attr(t, out, "http.request.method").AsString())
		require.Equal(t, "/v/42", attr(t, out, "url.path").AsString())
		require.EqualValues(t, http.StatusOK, requireInt64Attr(t, out, "http.response.status_code"))
		require.EqualValues(t, len("hello"), requireInt64Attr(t, out, "http.response.body.size"))
		require.GreaterOrEqual(t, requireInt64Attr(t, out, "server.elapsed_ns"), int64(0))
		requireNoAttr(t, out, "error.type")
		requireNoAttr(t, out, "error.message")
	})

	t.Run("puts the otx into the request context itself", func(t *testing.T) {
		h := newHarness(t)

		var (
			given *otx.Otx
			ok    bool
		)
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			given, ok = otx.FromOK(r.Context())
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.True(t, ok)
		require.Same(t, h.otx, given)
	})

	t.Run("writes both records when mounted outside the middleware, without span ids", func(t *testing.T) {
		h := newHarness(t)

		handler := otxhttp.BoundaryLogger(h.otx)(
			otxhttp.NewMiddleware(h.otx, "api")(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
			),
		)

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())
		require.Len(t, h.spans.Ended(), 1, "the server span is still started")

		// The records are written outside the span, so they cannot carry its
		// ids; the point of the regression is that they are written at all.
		requireNoSpanIDs(t, h.record(t, "HTTP in"))
		requireNoSpanIDs(t, h.record(t, "HTTP out"))
	})

	t.Run("writes both records with the ids of the server span when mounted inside the middleware", func(t *testing.T) {
		h := newHarness(t)

		var span_ctx trace.SpanContext
		handler := otxhttp.NewMiddleware(h.otx, "api")(
			otxhttp.BoundaryLogger(h.otx)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					span_ctx = trace.SpanContextFromContext(r.Context())
				}),
			),
		)

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())
		require.True(t, span_ctx.IsValid())

		requireSpanIDs(t, h.record(t, "HTTP in"), span_ctx)
		requireSpanIDs(t, h.record(t, "HTTP out"), span_ctx)
	})

	t.Run("writes both records when mounted on its own, without any otx in the context", func(t *testing.T) {
		h := newHarness(t)

		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

		// A bare context, as an http.Server hands one over.
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/v/42", nil)
		require.NoError(t, err)
		r.RemoteAddr = "10.0.0.1:1234"

		handler.ServeHTTP(httptest.NewRecorder(), r)

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())
	})

	t.Run("the status code decides the level of the closing record", func(t *testing.T) {
		for _, tc := range []struct {
			desc  string
			code  int
			level otellog.Severity
		}{
			{desc: "200 is logged at info", code: http.StatusOK, level: otellog.SeverityInfo},
			{desc: "404 is logged at warn", code: http.StatusNotFound, level: otellog.SeverityWarn},
			{desc: "418 is logged at warn", code: http.StatusTeapot, level: otellog.SeverityWarn},
			{desc: "500 is logged at error", code: http.StatusInternalServerError, level: otellog.SeverityError},
			{desc: "503 is logged at error", code: http.StatusServiceUnavailable, level: otellog.SeverityError},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.code)
				}))

				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

				out := h.record(t, "HTTP out")
				require.Equal(t, tc.level, out.Severity())
				require.EqualValues(t, tc.code, requireInt64Attr(t, out, "http.response.status_code"))

				// The opening record is always informational.
				in := h.record(t, "HTTP in")
				require.Equal(t, otellog.SeverityInfo, in.Severity())
			})
		}
	})

	t.Run("a panicking handler is logged and re-raised", func(t *testing.T) {
		h := newHarness(t)

		boom := errors.New("boom")
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic(boom)
		}))

		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil)
		require.PanicsWithValue(t, boom, func() {
			handler.ServeHTTP(httptest.NewRecorder(), r)
		}, "the panic must reach net/http.Server")

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityError, out.Severity())
		require.Equal(t, "*errors.errorString", attr(t, out, "error.type").AsString())
		require.Equal(t, "boom", attr(t, out, "error.message").AsString())

		// Current behaviour: the metrics of a panicking handler are lost,
		// because httpsnoop.CaptureMetrics never returns and so never assigns
		// them. The status code is reported as 0 rather than 500, and the
		// elapsed time as 0.
		require.EqualValues(t, 0, requireInt64Attr(t, out, "http.response.status_code"))
		require.EqualValues(t, 0, requireInt64Attr(t, out, "http.response.body.size"))
		require.EqualValues(t, 0, requireInt64Attr(t, out, "server.elapsed_ns"))
	})

	t.Run("a panic under the middleware is logged with the ids of the server span", func(t *testing.T) {
		h := newHarness(t)

		handler := otxhttp.NewMiddleware(h.otx, "api")(
			otxhttp.BoundaryLogger(h.otx)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					panic("kaboom")
				}),
			),
		)

		require.PanicsWithValue(t, "kaboom", func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))
		})

		spans := h.spans.Ended()
		require.Len(t, spans, 1, "the server span is ended even though the handler panicked")

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityError, out.Severity())
		require.Equal(t, "string", attr(t, out, "error.type").AsString())
		require.Equal(t, "kaboom", attr(t, out, "error.message").AsString())
		requireSpanIDs(t, out, spans[0].SpanContext())
	})

	t.Run("the peer is taken from the remote address", func(t *testing.T) {
		for _, tc := range []struct {
			desc        string
			remote_addr string
			peer_addr   string
			peer_port   int64
		}{
			{
				desc:        "an ipv6 peer is recorded without its brackets",
				remote_addr: "[::1]:8080",
				peer_addr:   "::1",
				peer_port:   8080,
			},
			{
				desc:        "an ipv6 peer with a zone is recorded without its brackets",
				remote_addr: "[fe80::1%eth0]:443",
				peer_addr:   "fe80::1%eth0",
				peer_port:   443,
			},
			{
				desc:        "an ipv4 peer is split into address and port",
				remote_addr: "127.0.0.1:9000",
				peer_addr:   "127.0.0.1",
				peer_port:   9000,
			},
			{
				desc:        "a remote address with no port is recorded whole",
				remote_addr: "/run/app.sock",
				peer_addr:   "/run/app.sock",
				peer_port:   -1,
			},
			{
				desc:        "an empty remote address is not recorded",
				remote_addr: "",
				peer_addr:   "",
				peer_port:   -1,
			},
			{
				desc:        "a non numeric port is dropped but the address is kept",
				remote_addr: "127.0.0.1:http",
				peer_addr:   "127.0.0.1",
				peer_port:   -1,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil)
				r.RemoteAddr = tc.remote_addr
				handler.ServeHTTP(httptest.NewRecorder(), r)

				for _, msg := range []string{"HTTP in", "HTTP out"} {
					record := h.record(t, msg)
					if tc.peer_addr == "" {
						requireNoAttr(t, record, "network.peer.address")
					} else {
						require.Equal(t, tc.peer_addr, attr(t, record, "network.peer.address").AsString())
					}
					if tc.peer_port < 0 {
						requireNoAttr(t, record, "network.peer.port")
					} else {
						require.Equal(t, tc.peer_port, requireInt64Attr(t, record, "network.peer.port"))
					}
				}
			})
		}
	})

	t.Run("the size and the elapsed time are int64 attributes", func(t *testing.T) {
		h := newHarness(t)

		body := []byte("0123456789")
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(body)
			w.Write(body)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.KindInt64, attr(t, out, "http.response.body.size").Kind())
		require.Equal(t, otellog.KindInt64, attr(t, out, "server.elapsed_ns").Kind())
		require.EqualValues(t, 2*len(body), requireInt64Attr(t, out, "http.response.body.size"))
	})

	t.Run("the elapsed time covers the handler and no more", func(t *testing.T) {
		h := newHarness(t)

		// Asserted against two real durations rather than against a constant:
		// the recorded value must be at least the time the handler provably
		// spent, and at most the time the whole call provably took.
		const held = 2 * time.Millisecond
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spin(held)
		}))

		t0 := time.Now()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v/42", nil))
		whole := time.Since(t0)

		elapsed := requireInt64Attr(t, h.record(t, "HTTP out"), "server.elapsed_ns")
		require.GreaterOrEqual(t, elapsed, held.Nanoseconds(), "the handler was held for at least this long")
		require.LessOrEqual(t, elapsed, whole.Nanoseconds(), "the whole call took no longer than this")
	})

	t.Run("the closing record is written even when the request context is cancelled", func(t *testing.T) {
		h := newHarness(t)

		ctx, cancel := context.WithCancel(t.Context())
		handler := otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The client hangs up mid-request.
			cancel()
			w.WriteHeader(http.StatusGatewayTimeout)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/v/42", nil))

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityError, out.Severity())
	})
}
