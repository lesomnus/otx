package otxgrpc_test

import (
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/stats"
)

const (
	method_get_feature   = "/routeguide.RouteGuide/GetFeature"
	method_list_features = "/routeguide.RouteGuide/ListFeatures"
)

// except answers with a filter that declines the named methods and accepts
// everything else.
func except(methods ...string) otxgrpc.Filter {
	return func(info *stats.RPCTagInfo) bool {
		for _, m := range methods {
			if info.FullMethodName == m {
				return false
			}
		}

		return true
	}
}

// filtering serves with a server logger that is given opts, and leaves the
// client side as it is so that a test can tell one side from the other.
func filtering(opts ...otxgrpc.Option) harnessOpt {
	return withServerHandlers(func(x *otx.Otx) []stats.Handler {
		return []stats.Handler{
			otxgrpc.NewServerHandler(x),
			otxgrpc.NewServerLogger(x, opts...),
		}
	})
}

func TestWithFilter(t *testing.T) {
	t.Run("an rpc the filter declines is not recorded", func(t *testing.T) {
		h := newHarness(t, filtering(otxgrpc.WithFilter(except(method_get_feature))))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		require.Empty(t, h.serverMessages())

		// And it is this handler that was told, not the RPC: the client logger
		// wrote its pair as it always does.
		require.Equal(t, []string{msg_req, msg_res}, h.clientMessages())
	})

	t.Run("an rpc it accepts is recorded as it was", func(t *testing.T) {
		h := newHarness(t, filtering(otxgrpc.WithFilter(except(method_get_feature))))

		require.NoError(t, h.listFeatures(t.Context()))
		h.served(t)

		require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
	})

	t.Run("a declined rpc is served with everything it was", func(t *testing.T) {
		h := newHarness(t, filtering(otxgrpc.WithFilter(except(method_get_feature))))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		// The Otx is in the context of the RPC, so a handler that logs on its
		// own still reaches the providers.
		given, ok := otx.FromOK(h.server_probe.Context())
		require.True(t, ok)
		require.Same(t, h.otx, given)

		// And the tracing handler decided for itself: what a filter says is
		// which records this handler writes.
		require.NotNil(t, h.span(t, trace.SpanKindServer))
	})

	t.Run("no filter is every rpc", func(t *testing.T) {
		for _, tc := range []struct {
			desc string
			opts []otxgrpc.Option
		}{
			{desc: "none given", opts: nil},
			// Ignored rather than read as a filter that accepts nothing, so a
			// caller that works one out need not check first.
			{desc: "a nil one", opts: []otxgrpc.Option{otxgrpc.WithFilter(nil)}},
			{desc: "one that accepts", opts: []otxgrpc.Option{otxgrpc.WithFilter(except(method_list_features))}},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t, filtering(tc.opts...))

				require.NoError(t, h.getFeature(t.Context()))
				h.served(t)

				require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
			})
		}
	})

	t.Run("the client logger takes one too", func(t *testing.T) {
		h := newHarness(t, withClientHandlers(func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{
				otxgrpc.NewClientHandler(x),
				otxgrpc.NewClientLogger(x, otxgrpc.WithFilter(except(method_get_feature))),
			}
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		require.Empty(t, h.clientMessages())
		require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
	})
}
