package otxhttp_test

import (
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxhttp"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// spanNamed returns the one ended span with the given name.
func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	var (
		found sdktrace.ReadOnlySpan
		n     int
	)
	for _, s := range spans {
		if s.Name() != name {
			continue
		}

		found = s
		n++
	}

	require.Equalf(t, 1, n, "want exactly one %q span, got %d", name, n)
	return found
}

// serveOnce mounts handler on a real [net/http.Server] listening on a real
// socket and returns its URL together with a channel that is closed once the
// server side of a request is completely finished.
//
// The channel is what lets a test read the server side records without a
// sleep: the closing record of the boundary logger is written by a deferred
// call inside ServeHTTP, and this defer is registered further out, so it runs
// after it - on the way out of a panic as well as on the way out of a return.
func serveOnce(t *testing.T, handler http.Handler) (string, <-chan struct{}) {
	t.Helper()

	done := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		handler.ServeHTTP(w, r)
	}))

	// net/http logs a recovered panic and its stack to this logger. The tests
	// below assert on the panic themselves and do not need it on stderr.
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)

	return srv.URL, done
}

func TestEndToEnd(t *testing.T) {
	t.Run("a real client and a real server share one trace", func(t *testing.T) {
		server := newHarness(t)
		client := newHarness(t)

		var (
			handler_span trace.SpanContext
			remote_addr  string
		)
		url, done := serveOnce(t, otxhttp.NewHandler(server.otx,
			otxhttp.BoundaryLogger(server.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler_span = trace.SpanContextFromContext(r.Context())
				remote_addr = r.RemoteAddr
				log.From(r.Context()).Info("in the handler")

				// Set rather than left to net/http, so that the header the
				// client records is one this test chose.
				w.Header().Set("Content-Length", "4")
				w.Write([]byte("pong"))
			})),
			"api",
		))

		// A nil base, so the request really goes out over
		// http.DefaultTransport and across a socket.
		c := &http.Client{Transport: otxhttp.NewTransport(client.otx, nil)}

		ctx, span := client.otx.TraceStart(t.Context(), "caller")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/ping", nil)
		require.NoError(t, err)

		res, err := c.Do(req)
		require.NoError(t, err)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		span.End()

		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, "pong", string(body))

		<-done

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, client.messages())
		require.Equal(t, []string{"HTTP in", "in the handler", "HTTP out"}, server.messages())

		// The trace crosses the wire: the server span is a child of the client
		// span, in the trace the caller started.
		client_span := spanNamed(t, client.spans.Ended(), "HTTP GET")
		require.Equal(t, trace.SpanKindClient, client_span.SpanKind())
		require.Equal(t, span.SpanContext().SpanID(), client_span.Parent().SpanID())

		server_span := spanNamed(t, server.spans.Ended(), "api")
		require.Equal(t, trace.SpanKindServer, server_span.SpanKind())
		require.Equal(t, client_span.SpanContext().TraceID(), server_span.SpanContext().TraceID())
		require.Equal(t, client_span.SpanContext().SpanID(), server_span.Parent().SpanID())
		require.Equal(t, server_span.SpanContext(), handler_span)

		// Every record on either side carries the ids of the span of its own
		// process.
		for _, msg := range []string{"HTTP req", "HTTP res", "HTTP end"} {
			requireSpanIDs(t, client.record(t, msg), client_span.SpanContext())
		}
		for _, msg := range []string{"HTTP in", "in the handler", "HTTP out"} {
			requireSpanIDs(t, server.record(t, msg), server_span.SpanContext())
		}

		// The peer of the server side records is the far end of the real
		// connection, split into address and port.
		host, port, err := net.SplitHostPort(remote_addr)
		require.NoError(t, err)
		peer_port, err := strconv.Atoi(port)
		require.NoError(t, err)

		in := server.record(t, "HTTP in")
		require.Equal(t, http.MethodGet, attr(t, in, "http.request.method").AsString())
		require.Equal(t, "/ping", attr(t, in, "url.path").AsString())
		require.Equal(t, host, attr(t, in, "network.peer.address").AsString())
		require.EqualValues(t, peer_port, requireInt64Attr(t, in, "network.peer.port"))

		out := server.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityInfo, out.Severity())
		require.EqualValues(t, http.StatusOK, requireInt64Attr(t, out, "http.response.status_code"))
		require.EqualValues(t, len("pong"), requireInt64Attr(t, out, "http.response.body.size"))

		client_res := client.record(t, "HTTP res")
		require.Equal(t, url+"/ping", attr(t, client_res, "url.full").AsString())
		require.EqualValues(t, http.StatusOK, requireInt64Attr(t, client_res, "http.response.status_code"))
		require.Equal(t, "4", attr(t, client_res, "http.response.header.content-length").AsString())

		client_end := client.record(t, "HTTP end")
		require.EqualValues(t, len("pong"), requireInt64Attr(t, client_end, "http.response.body.size"))
	})

	t.Run("a panic escapes into the recovery of net/http.Server", func(t *testing.T) {
		h := newHarness(t)

		url, done := serveOnce(t, otxhttp.NewHandler(h.otx,
			otxhttp.BoundaryLogger(h.otx)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic("kaboom")
			})),
			"api",
		))

		// A plain client: what is under test is the server side, and what
		// reaches the client is a dropped connection rather than a response,
		// which is the whole point of re-raising instead of swallowing.
		res, err := http.Get(url + "/boom")
		require.Error(t, err, "net/http.Server closes the connection on an unrecovered panic")
		require.Nil(t, res)

		<-done

		require.Equal(t, []string{"HTTP in", "HTTP out"}, h.messages())

		out := h.record(t, "HTTP out")
		require.Equal(t, otellog.SeverityError, out.Severity())
		require.Equal(t, "string", attr(t, out, "error.type").AsString())
		require.Equal(t, "kaboom", attr(t, out, "error.message").AsString())

		// The server span is still ended, and the closing record still carries
		// its ids.
		span := spanNamed(t, h.spans.Ended(), "api")
		requireSpanIDs(t, out, span.SpanContext())
	})
}
