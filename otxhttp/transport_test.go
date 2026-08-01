package otxhttp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxhttp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	otellog "go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newResponse is a response as a [net/http.RoundTripper] hands one back.
func newResponse(code int, body io.ReadCloser) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          body,
		ContentLength: -1,
	}
}

// rwBody is a body that is also an [io.Writer], as net/http hands back for a
// 101 Switching Protocols response.
type rwBody struct {
	io.Reader
	written bytes.Buffer
	closed  bool
}

var _ io.ReadWriteCloser = (*rwBody)(nil)

func (b *rwBody) Write(p []byte) (int, error) {
	return b.written.Write(p)
}

func (b *rwBody) Close() error {
	b.closed = true
	return nil
}

// wrappedEOFBody yields its content once and then reports an [io.EOF] that is
// wrapped rather than returned bare, as a body that decodes or decompresses on
// the way through does.
type wrappedEOFBody struct {
	content string
	read    bool
	closed  bool
}

func (b *wrappedEOFBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, fmt.Errorf("read body: %w", io.EOF)
	}

	b.read = true
	return copy(p, b.content), nil
}

func (b *wrappedEOFBody) Close() error {
	b.closed = true
	return nil
}

// dialError gives the error path of the transport a distinguishable type.
type dialError struct {
	msg string
}

func (e *dialError) Error() string {
	return e.msg
}

func TestNewTransport(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxhttp: NewTransport called with a nil *otx.Otx", func() {
			otxhttp.NewTransport(nil, nil)
		})
	})

	t.Run("falls back to the default transport", func(t *testing.T) {
		h := newHarness(t)

		// A nil base is only observably http.DefaultTransport if the request
		// actually reaches a server through it; a non-nil return value would
		// hold just as well with the fallback deleted, and calling RoundTrip
		// on a nil base panics rather than failing.
		var reached int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached++
			w.Write([]byte("pong"))
		}))
		t.Cleanup(srv.Close)

		rt := otxhttp.NewTransport(h.otx, nil)

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Equal(t, 1, reached, "the request must have gone out over http.DefaultTransport")
		require.Equal(t, "pong", string(body))
		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
	})

	t.Run("options given by the caller win over the defaults", func(t *testing.T) {
		h := newHarness(t)

		// As in the middleware, the option that decides this is one the
		// defaults also set: the spans land in exactly one of the two
		// recorders, and that says which slice was applied last.
		other := tracetest.NewSpanRecorder()
		other_provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(other))
		t.Cleanup(func() {
			require.NoError(t, other_provider.Shutdown(context.Background()))
		})

		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("."))), nil
		}), otelhttp.WithTracerProvider(other_provider))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Empty(t, h.spans.Ended(), "the tracer provider of the otx must lose to the one the caller gave")

		spans := other.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, trace.SpanKindClient, spans[0].SpanKind())
	})

	t.Run("the client metrics go to the meter provider of the otx", func(t *testing.T) {
		meter := &meterRecorder{}
		h := newHarness(t, otx.WithMeterProvider(meter))

		const held = 2 * time.Millisecond
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			spin(held)
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("."))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		t0 := time.Now()
		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		whole := time.Since(t0)

		// Without the wiring otelhttp would take the global meter provider and
		// this one would only ever see the scope of the Otx itself.
		require.Contains(t, meter.scopeNames(), otelhttp.ScopeName)
		require.Contains(t, meter.instrumentNames(), "http.client.request.duration")

		durations := meter.valuesOf("http.client.request.duration")
		require.Len(t, durations, 1, "one request, one duration")
		require.GreaterOrEqual(t, durations[0], held.Seconds(), "the duration is in seconds and covers the round trip")
		require.LessOrEqual(t, durations[0], whole.Seconds())
	})

	t.Run("writes a record as the request is sent, as the head arrives and as the body ends", func(t *testing.T) {
		h := newHarness(t)

		content := "hello world"
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, []string{"HTTP req"}, h.messages(), `"HTTP req" must be written before the request leaves`)
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader(content))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x?q=1", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages(), `"HTTP end" must wait for the body`)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Equal(t, content, string(body))
		require.NoError(t, res.Body.Close())

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())

		req := h.record(t, "HTTP req")
		require.Equal(t, otellog.SeverityInfo, req.Severity())
		require.Equal(t, http.MethodGet, attr(t, req, "http.request.method").AsString())
		require.Equal(t, "https://example.com/x?q=1", attr(t, req, "url.full").AsString())
		requireNoAttr(t, req, "http.response.status_code")

		res_record := h.record(t, "HTTP res")
		require.Equal(t, otellog.SeverityInfo, res_record.Severity())
		require.EqualValues(t, http.StatusOK, requireInt64Attr(t, res_record, "http.response.status_code"))
		require.GreaterOrEqual(t, requireInt64Attr(t, res_record, "client.elapsed_ns"), int64(0))
		requireNoAttr(t, res_record, "http.response.body.size")
		// The response carries no Content-Length, so neither may the record:
		// an attribute with an empty value is not the same as no attribute.
		requireNoAttr(t, res_record, "http.response.header.content-length")

		end := h.record(t, "HTTP end")
		require.Equal(t, otellog.SeverityInfo, end.Severity())
		require.Equal(t, "https://example.com/x?q=1", attr(t, end, "url.full").AsString())
		require.EqualValues(t, len(content), requireInt64Attr(t, end, "http.response.body.size"))
		require.GreaterOrEqual(t, requireInt64Attr(t, end, "client.elapsed_ns"), int64(0))
	})

	t.Run("records the url with any userinfo stripped", func(t *testing.T) {
		h := newHarness(t)

		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("."))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://user:secret@example.com/x?q=1", nil)
		require.NoError(t, err)
		require.NotNil(t, r.URL.User, "the request under test must actually carry userinfo")

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
		for _, msg := range []string{"HTTP req", "HTTP res", "HTTP end"} {
			record := h.record(t, msg)
			require.Equal(t, "https://example.com/x?q=1", attr(t, record, "url.full").AsString())
			requireNoAttr(t, record, "url.original")
		}

		// Nothing anywhere may leak the password.
		for _, record := range h.records() {
			record.WalkAttributes(func(kv otellog.KeyValue) bool {
				require.NotContains(t, kv.Value.String(), "secret", "attribute %q leaks the userinfo", kv.Key)
				return true
			})
			require.NotContains(t, record.Body().AsString(), "secret")
		}
	})

	t.Run("records the status code exactly once", func(t *testing.T) {
		h := newHarness(t)

		// The SDK keeps only the last attribute of a given key, so a duplicate
		// is only visible one layer up, at the log API.
		api := &apiRecorder{}
		x := otx.New(otx.WithLoggerProvider(api))

		newRes := func() *http.Response {
			res := newResponse(http.StatusNotFound, io.NopCloser(strings.NewReader("nope")))
			res.Header.Set("Content-Length", "4")
			return res
		}

		for _, x := range []*otx.Otx{h.otx, x} {
			rt := otxhttp.NewTransport(x, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return newRes(), nil
			}))

			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)
			require.NoError(t, res.Body.Close())
		}

		api_res := api.record(t, "HTTP res")
		require.Equal(t, 1, apiAttrCount(api_res, "http.response.status_code"))

		api_end := api.record(t, "HTTP end")
		require.Equal(t, 1, apiAttrCount(api_end, "http.response.status_code"))

		api_req := api.record(t, "HTTP req")
		require.Zero(t, apiAttrCount(api_req, "http.response.status_code"))

		res_record := h.record(t, "HTTP res")
		require.EqualValues(t, http.StatusNotFound, requireInt64Attr(t, res_record, "http.response.status_code"))
		require.Equal(t, otellog.SeverityWarn, res_record.Severity(), "a 4xx is logged at warn")
		require.Equal(t, "4", attr(t, res_record, "http.response.header.content-length").AsString())

		end := h.record(t, "HTTP end")
		require.EqualValues(t, http.StatusNotFound, requireInt64Attr(t, end, "http.response.status_code"))
		requireNoAttr(t, end, "http.response.header.content-length")
	})

	t.Run("a nil body is left alone", func(t *testing.T) {
		h := newHarness(t)

		t.Run("by the logger", func(t *testing.T) {
			h.logs.Reset()

			rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return newResponse(http.StatusNoContent, nil), nil
			}))

			r, err := http.NewRequestWithContext(t.Context(), http.MethodHead, "https://example.com/x", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)
			require.Nil(t, res.Body)
			require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages())
		})

		t.Run("and closing it does not panic", func(t *testing.T) {
			h.logs.Reset()

			rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return newResponse(http.StatusNoContent, nil), nil
			}))

			r, err := http.NewRequestWithContext(t.Context(), http.MethodHead, "https://example.com/x", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)
			require.NotPanics(t, func() {
				require.NoError(t, res.Body.Close())
			})
			require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages(), "there is no body to end")
		})
	})

	t.Run("http.NoBody is left as is", func(t *testing.T) {
		h := newHarness(t)

		rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusNotModified, http.NoBody), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.True(t, res.Body == http.NoBody, "the identity of http.NoBody matters to HEAD, 204 and 304 handling")

		require.NoError(t, res.Body.Close())
		require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages())
	})

	t.Run("a switched protocol body stays writable", func(t *testing.T) {
		t.Run("through the logger", func(t *testing.T) {
			h := newHarness(t)

			body := &rwBody{Reader: strings.NewReader("from the server")}
			rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return newResponse(http.StatusSwitchingProtocols, body), nil
			}))

			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/ws", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)

			rw, ok := res.Body.(io.ReadWriteCloser)
			require.True(t, ok, "a 101 body must remain an io.ReadWriteCloser")

			n, err := rw.Write([]byte("ping"))
			require.NoError(t, err)
			require.Equal(t, 4, n)
			require.Equal(t, "ping", body.written.String(), "the write must reach the underlying writer")

			require.NoError(t, rw.Close())
			require.True(t, body.closed)
			require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
		})

		t.Run("through the full transport", func(t *testing.T) {
			h := newHarness(t)

			body := &rwBody{Reader: strings.NewReader("from the server")}
			rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return newResponse(http.StatusSwitchingProtocols, body), nil
			}))

			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/ws", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)

			rw, ok := res.Body.(io.ReadWriteCloser)
			require.True(t, ok, "a 101 body must remain an io.ReadWriteCloser")

			_, err = rw.Write([]byte("ping"))
			require.NoError(t, err)
			require.Equal(t, "ping", body.written.String())

			read := make([]byte, 4)
			n, err := rw.Read(read)
			require.NoError(t, err)
			require.Equal(t, "from", string(read[:n]))

			require.NoError(t, rw.Close())

			end := h.record(t, "HTTP end")
			require.EqualValues(t, 4, requireInt64Attr(t, end, "http.response.body.size"))
		})
	})

	t.Run("a wrapped io.EOF still ends the body", func(t *testing.T) {
		h := newHarness(t)

		body := &wrappedEOFBody{content: "0123456789"}
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, body), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)

		buf := make([]byte, 64)
		n, err := res.Body.Read(buf)
		require.NoError(t, err)
		require.Equal(t, body.content, string(buf[:n]))
		require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages())

		_, err = res.Body.Read(buf)
		require.ErrorIs(t, err, io.EOF)
		require.NotEqual(t, io.EOF, err, "the point of this test is an EOF that is not comparable with ==")

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())

		end := h.record(t, "HTTP end")
		require.EqualValues(t, len(body.content), requireInt64Attr(t, end, "http.response.body.size"))

		// Closing afterwards must not write a second one.
		require.NoError(t, res.Body.Close())
		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
	})

	t.Run("the body ends exactly once", func(t *testing.T) {
		for _, tc := range []struct {
			desc string
			use  func(t *testing.T, body io.ReadCloser)
			size int64
		}{
			{
				desc: "when it is read to the end",
				use: func(t *testing.T, body io.ReadCloser) {
					b, err := io.ReadAll(body)
					require.NoError(t, err)
					require.Equal(t, "0123456789", string(b))
				},
				size: 10,
			},
			{
				desc: "when it is closed unread",
				use: func(t *testing.T, body io.ReadCloser) {
					require.NoError(t, body.Close())
				},
				size: 0,
			},
			{
				desc: "when it is read to the end and then closed",
				use: func(t *testing.T, body io.ReadCloser) {
					_, err := io.ReadAll(body)
					require.NoError(t, err)
					require.NoError(t, body.Close())
					require.NoError(t, body.Close())
				},
				size: 10,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("0123456789"))), nil
				}))

				r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
				require.NoError(t, err)

				res, err := rt.RoundTrip(r)
				require.NoError(t, err)

				tc.use(t, res.Body)

				require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())

				end := h.record(t, "HTTP end")
				require.Equal(t, tc.size, requireInt64Attr(t, end, "http.response.body.size"))
			})
		}
	})

	t.Run("a body that is neither read nor closed never ends", func(t *testing.T) {
		h := newHarness(t)

		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("0123456789"))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		_, err = rt.RoundTrip(r)
		require.NoError(t, err)

		require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages())
	})

	t.Run("a failed round trip is logged at warn with the error in an attribute", func(t *testing.T) {
		h := newHarness(t)

		err_want := &dialError{msg: "dial tcp 10.0.0.1:443: connect: connection refused"}
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return nil, err_want
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.ErrorIs(t, err, err_want)
		require.Nil(t, res)

		require.Equal(t, []string{"HTTP req", "HTTP err"}, h.messages())

		record := h.record(t, "HTTP err")
		require.Equal(t, otellog.SeverityWarn, record.Severity())
		require.Equal(t, "*otxhttp_test.dialError", attr(t, record, "error.type").AsString())
		require.Equal(t, err_want.msg, attr(t, record, "error.message").AsString())
		require.Equal(t, "https://example.com/x", attr(t, record, "url.full").AsString())
		require.Equal(t, otellog.KindInt64, attr(t, record, "client.elapsed_ns").Kind())
		requireNoAttr(t, record, "http.response.status_code")

		// The message must stay cardinality free: the error text belongs in the
		// attribute, not in the body.
		require.Equal(t, "HTTP err", record.Body().AsString())
		require.NotContains(t, record.Body().AsString(), err_want.msg)
		require.Equal(t, "WARN", record.SeverityText())
	})

	t.Run("the elapsed time covers the round trip and no more", func(t *testing.T) {
		h := newHarness(t)

		// Asserted against two real durations rather than against a constant:
		// the recorded value must be at least the time the round trip provably
		// took, and at most the time the whole call provably took.
		const held = 2 * time.Millisecond
		rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			spin(held)
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("0123456789"))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		t0 := time.Now()
		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		head := time.Since(t0)

		spin(held)
		require.NoError(t, res.Body.Close())
		whole := time.Since(t0)

		on_res := requireInt64Attr(t, h.record(t, "HTTP res"), "client.elapsed_ns")
		require.GreaterOrEqual(t, on_res, held.Nanoseconds(), "the round trip was held for at least this long")
		require.LessOrEqual(t, on_res, head.Nanoseconds(), "the head arrived within this")

		// Both are measured from the same start, so the one written when the
		// body ends can only be the larger of the two.
		on_end := requireInt64Attr(t, h.record(t, "HTTP end"), "client.elapsed_ns")
		require.Greater(t, on_end, on_res, "the body outlives the head")
		require.LessOrEqual(t, on_end, whole.Nanoseconds())
	})

	t.Run("concurrent round trips through one transport keep their records apart", func(t *testing.T) {
		h := newHarness(t)

		const n = 16
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader(r.URL.Path))), nil
		}))

		// Errors are collected rather than asserted in place: require.NoError
		// off the test goroutine would only unwind the goroutine it is on.
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("https://example.com/%d", i), nil)
				if err != nil {
					errs[i] = err
					return
				}

				res, err := rt.RoundTrip(r)
				if err != nil {
					errs[i] = err
					return
				}

				_, err = io.ReadAll(res.Body)
				errs[i] = errors.Join(err, res.Body.Close())
			})
		}
		wg.Wait()
		require.NoError(t, errors.Join(errs...))

		counts := map[string]int{}
		for _, msg := range h.messages() {
			counts[msg]++
		}
		require.Equal(t, map[string]int{"HTTP req": n, "HTTP res": n, "HTTP end": n}, counts)

		// Each record must carry the url of its own request, not one it raced
		// with: the attribute slices are per round trip and must stay so.
		urls := map[string][]string{}
		for _, record := range h.records() {
			msg := record.Body().AsString()
			urls[msg] = append(urls[msg], attr(t, record, "url.full").AsString())
		}
		want := make([]string, n)
		for i := range n {
			want[i] = fmt.Sprintf("https://example.com/%d", i)
		}
		for msg, got := range urls {
			require.ElementsMatch(t, want, got, "the %q records must cover every request exactly once", msg)
		}
	})

	t.Run("a body read and closed at once ends exactly once", func(t *testing.T) {
		h := newHarness(t)

		// The logger on its own: a body wrapped by otelhttp as well is no
		// safer than the one it wraps, and the wrapper under test is the one
		// this package owns.
		rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("0123456789"))), nil
		}))

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)

		// Draining a body while another goroutine closes it is what net/http
		// does to a response whose request context is cancelled. Only one of
		// the two may write the closing record.
		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = io.ReadAll(res.Body)
		})
		for range 8 {
			wg.Go(func() {
				_ = res.Body.Close()
			})
		}
		wg.Wait()

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
	})

	t.Run("the records are written even when the request context is cancelled", func(t *testing.T) {
		t.Run("when the response arrives anyway", func(t *testing.T) {
			h := newHarness(t)

			ctx, cancel := context.WithCancel(t.Context())
			rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				// The caller gives up while the request is in flight.
				cancel()
				return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("0123456789"))), nil
			}))

			r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/x", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.NoError(t, err)

			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.NoError(t, res.Body.Close())

			require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
			require.EqualValues(t, len(body), requireInt64Attr(t, h.record(t, "HTTP end"), "http.response.body.size"))
		})

		t.Run("when the round trip fails because of it", func(t *testing.T) {
			h := newHarness(t)

			ctx, cancel := context.WithCancel(t.Context())
			rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				cancel()
				return nil, r.Context().Err()
			}))

			r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/x", nil)
			require.NoError(t, err)

			res, err := rt.RoundTrip(r)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, res)

			require.Equal(t, []string{"HTTP req", "HTTP err"}, h.messages())
			require.Equal(t, context.Canceled.Error(), attr(t, h.record(t, "HTTP err"), "error.message").AsString())
		})
	})

	t.Run("the otx reaches a request built from a bare context", func(t *testing.T) {
		h := newHarness(t)

		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("."))), nil
		}))

		// http.NewRequest builds on context.Background, which carries no Otx;
		// without the injection in the transport the records would go to the
		// global logger provider and be discarded.
		r, err := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)
		require.Equal(t, context.Background(), r.Context())

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())

		// And so does the tracer provider. otelhttp only falls back to the
		// provider of the span in the request context; a bare context has no
		// span, so a client span in this recorder can only have come from the
		// provider the transport handed over.
		spans := h.spans.Ended()
		require.Len(t, spans, 1)
		require.Equal(t, "HTTP GET", spans[0].Name())
		require.Equal(t, trace.SpanKindClient, spans[0].SpanKind())
		require.False(t, spans[0].Parent().IsValid(), "there was no parent span to inherit from")

		for _, msg := range []string{"HTTP req", "HTTP res", "HTTP end"} {
			requireSpanIDs(t, h.record(t, msg), spans[0].SpanContext())
		}
	})

	t.Run("the trace context is written on the outbound request", func(t *testing.T) {
		h := newHarness(t)

		var seen http.Header
		rt := otxhttp.NewTransport(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			seen = r.Header.Clone()
			return newResponse(http.StatusOK, io.NopCloser(strings.NewReader("."))), nil
		}))

		ctx, span := h.otx.TraceStart(t.Context(), "caller")
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		span.End()

		traceparent := seen.Get("traceparent")
		require.NotEmpty(t, traceparent, "the propagator of the otx must inject the trace context")
		require.Contains(t, traceparent, span.SpanContext().TraceID().String())

		// Every record carries the ids of the client span otelhttp started.
		for _, msg := range []string{"HTTP req", "HTTP res", "HTTP end"} {
			record := h.record(t, msg)
			require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
			require.Contains(t, traceparent, record.SpanID().String())
		}

		ended := h.spans.Ended()
		require.Len(t, ended, 2)
		require.Equal(t, "HTTP GET", ended[0].Name())
		require.Equal(t, trace.SpanKindClient, ended[0].SpanKind())
		require.Equal(t, "caller", ended[1].Name())
	})
}

func TestNewTransportLogger(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxhttp: NewTransportLogger called with a nil *otx.Otx", func() {
			otxhttp.NewTransportLogger(nil, nil)
		})
	})

	t.Run("falls back to the default transport", func(t *testing.T) {
		h := newHarness(t)

		var reached int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached++
			w.Write([]byte("pong"))
		}))
		t.Cleanup(srv.Close)

		rt := otxhttp.NewTransportLogger(h.otx, nil)

		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/x", nil)
		require.NoError(t, err)

		res, err := rt.RoundTrip(r)
		require.NoError(t, err)

		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Equal(t, 1, reached, "the request must have gone out over http.DefaultTransport")
		require.Equal(t, "pong", string(body))
		require.Equal(t, []string{"HTTP req", "HTTP res", "HTTP end"}, h.messages())
	})

	t.Run("the zero value falls back to the otx in the request context", func(t *testing.T) {
		h := newHarness(t)

		// Neither constructor can produce a TransportLogger with a nil otx, so
		// this is the only way to reach the nil guard in RoundTrip. It is
		// still expected to log, through whatever the request context carries.
		rt := &otxhttp.TransportLogger{}

		r, err := http.NewRequestWithContext(otx.Into(t.Context(), h.otx), http.MethodGet, "https://example.com/x", nil)
		require.NoError(t, err)

		// There is no base either, so the call cannot get past the round trip.
		require.Panics(t, func() {
			_, _ = rt.RoundTrip(r)
		})

		require.Equal(t, []string{"HTTP req"}, h.messages(), "the opening record is written before the base is touched")
		require.Equal(t, "https://example.com/x", attr(t, h.record(t, "HTTP req"), "url.full").AsString())
	})

	t.Run("records an empty url for a request that has none", func(t *testing.T) {
		h := newHarness(t)

		rt := otxhttp.NewTransportLogger(h.otx, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, http.NoBody), nil
		}))

		r := (&http.Request{Method: http.MethodGet, Header: http.Header{}}).WithContext(t.Context())
		res, err := rt.RoundTrip(r)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())

		require.Equal(t, []string{"HTTP req", "HTTP res"}, h.messages())

		req := h.record(t, "HTTP req")
		require.Empty(t, attr(t, req, "url.full").AsString())
	})
}
