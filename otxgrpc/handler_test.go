package otxgrpc_test

import (
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/stats"
)

func TestNewServerHandler(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxgrpc: NewServerHandler called with a nil *otx.Otx", func() {
			otxgrpc.NewServerHandler(nil)
		})
	})

	t.Run("puts the otx into the context the service implementation is given", func(t *testing.T) {
		h := newHarness(t, withServerHandlers(func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{otxgrpc.NewServerHandler(x)}
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		given, ok := otx.FromOK(h.service.Context())
		require.True(t, ok, "the service implementation must find the otx in its context")
		require.Same(t, h.otx, given)
	})

	t.Run("a handler registered before it still finds the otx", func(t *testing.T) {
		// The connection context is the parent of every RPC context on it, so
		// TagConn reaches a handler whose TagRPC runs first.
		first := newProbe()
		h := newHarness(t, withServerHandlers(func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{first, otxgrpc.NewServerHandler(x)}
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		given, ok := otx.FromOK(first.Context())
		require.True(t, ok)
		require.Same(t, h.otx, given)
	})

	t.Run("puts the otx into the context of every rpc kind", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t, withServerHandlers(func(x *otx.Otx) []stats.Handler {
					return []stats.Handler{otxgrpc.NewServerHandler(x)}
				}))

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				given, ok := otx.FromOK(h.service.Context())
				require.True(t, ok)
				require.Same(t, h.otx, given)
			})
		}
	})

	t.Run("starts a server span", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		span := h.span(t, trace.SpanKindServer)
		require.Equal(t, "routeguide.RouteGuide/GetFeature", span.Name())
	})

	t.Run("an option wins over the providers of the otx", func(t *testing.T) {
		other, _, spans := newOtx(t)

		h := newHarness(t,
			withServerHandlers(func(x *otx.Otx) []stats.Handler {
				return []stats.Handler{
					otxgrpc.NewServerHandler(x, otelgrpc.WithTracerProvider(other.Providers().Tracer())),
				}
			}),
			withClientHandlers(func(x *otx.Otx) []stats.Handler {
				return nil
			}),
		)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		require.Len(t, spans.Ended(), 1, "the server span went to the tracer provider of the option")
		require.Empty(t, h.spans.Ended(), "and not to the one of the otx")
	})
}

func TestNewClientHandler(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxgrpc: NewClientHandler called with a nil *otx.Otx", func() {
			otxgrpc.NewClientHandler(nil)
		})
	})

	t.Run("puts the otx into the rpc context", func(t *testing.T) {
		h := newHarness(t, withClientHandlers(func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{otxgrpc.NewClientHandler(x)}
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		// The RPC context of a client is not reachable from the caller, so it is
		// read from the probe the harness registers after the handler.
		given, ok := otx.FromOK(h.client_probe.Context())
		require.True(t, ok, "a handler registered after this one must find the otx")
		require.Same(t, h.otx, given)
	})

	t.Run("puts the otx into the rpc context even when the call was made from a bare context", func(t *testing.T) {
		h := newHarness(t, withClientHandlers(func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{otxgrpc.NewClientHandler(x)}
		}))

		// t.Context carries no otx: nothing on the caller side put one there.
		_, ok := otx.FromOK(t.Context())
		require.False(t, ok)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		given, ok := otx.FromOK(h.client_probe.Context())
		require.True(t, ok)
		require.Same(t, h.otx, given)
	})

	t.Run("starts a client span", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		span := h.span(t, trace.SpanKindClient)
		require.Equal(t, "routeguide.RouteGuide/GetFeature", span.Name())
	})
}

func TestTracePropagation(t *testing.T) {
	t.Run("the server span continues the trace of the client span", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				client := h.span(t, trace.SpanKindClient)
				server := h.span(t, trace.SpanKindServer)

				require.True(t, server.Parent().IsRemote(), "the parent of the server span comes over the wire")
				require.Equal(t, client.SpanContext().SpanID(), server.Parent().SpanID())
				require.Equal(t, client.SpanContext().TraceID(), server.SpanContext().TraceID(),
					"both spans belong to one trace")
				require.NotEqual(t, client.SpanContext().SpanID(), server.SpanContext().SpanID())
			})
		}
	})

	t.Run("the server span is a root when the client is not instrumented", func(t *testing.T) {
		h := newHarness(t, withClientHandlers(func(x *otx.Otx) []stats.Handler {
			return nil
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		server := h.span(t, trace.SpanKindServer)
		require.False(t, server.Parent().IsValid(), "there is no trace context to continue")
	})

	t.Run("the records of both sides carry the ids of the span of their side", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		client := h.span(t, trace.SpanKindClient).SpanContext()
		server := h.span(t, trace.SpanKindServer).SpanContext()

		requireSpanIDs(t, h.record(t, msg_req), client)
		requireSpanIDs(t, h.record(t, msg_res), client)
		requireSpanIDs(t, h.record(t, msg_in), server)
		requireSpanIDs(t, h.record(t, msg_out), server)

		require.Equal(t, client.TraceID(), server.TraceID())
	})
}
