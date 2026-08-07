package otxgrpc_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"github.com/lesomnus/otx/otxgrpc/internal/routeguide"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// The address a bufconn peer reports on both ends; it has no port, so the
// records carry an address but no network.peer.port.
const peer_addr = "bufconn"

// fakeAddr is a peer address in whatever shape a test needs, for the events a
// test hands to a handler itself.
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestNewServerLogger(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxgrpc: NewServerLogger called with a nil *otx.Otx", func() {
			otxgrpc.NewServerLogger(nil)
		})
	})

	t.Run("writes one record as the rpc arrives and one as it completes", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
			})
		}
	})

	t.Run("the opening record is written before the service implementation runs", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		h := newHarness(t, withService(&routeguide.Server{
			NumSend: num_res,
			Entered: entered,
		}))

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)
		<-entered

		require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
	})

	t.Run("the opening record of a unary or server streaming rpc comes from the first payload", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				in := h.record(t, msg_in)
				if tc.client_stream {
					// Nothing has arrived yet when the record is written from
					// stats.Begin, so it carries no message size.
					requireNoAttr(t, in, "rpc.message.compressed_size")
					requireNoAttr(t, in, "rpc.message.uncompressed_size")

					return
				}

				// Written from the first stats.InPayload, which is the single
				// request of a unary or server streaming RPC.
				require.Positive(t, requireInt64Attr(t, in, "rpc.message.compressed_size"))
				require.Positive(t, requireInt64Attr(t, in, "rpc.message.uncompressed_size"))
			})
		}
	})

	t.Run("every record names the service and the method and carries the ids of the server span", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				for _, msg := range []string{msg_in, msg_out} {
					r := h.record(t, msg)
					requireMethod(t, r, tc.full_method)
					requireValidSpanIDs(t, r)
				}
			})
		}
	})

	t.Run("the peer address is on the opening record and on the closing one", func(t *testing.T) {
		// A regression: the closing record used to lose the address that
		// stats.InHeader had established, because a later event without one
		// overwrote it.
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				for _, msg := range []string{msg_in, msg_out} {
					r := h.record(t, msg)
					require.Equal(t, peer_addr, attr(t, r, "network.peer.address").AsString())
					requireNoAttr(t, r, "network.peer.port")
				}
			})
		}
	})

	t.Run("the peer address and port are split out of a real network address", func(t *testing.T) {
		h := newHarness(t, overTCP())

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		for _, msg := range []string{msg_in, msg_out} {
			r := h.record(t, msg)
			require.Equal(t, "127.0.0.1", attr(t, r, "network.peer.address").AsString())
			require.Positive(t, requireInt64Attr(t, r, "network.peer.port"))
		}
	})

	t.Run("the peer is taken from the address of the incoming header", func(t *testing.T) {
		for _, tc := range []struct {
			desc        string
			remote_addr net.Addr
			peer_addr   string
			peer_port   int64
		}{
			{
				desc:        "an ipv4 peer is split into address and port",
				remote_addr: fakeAddr("127.0.0.1:9000"),
				peer_addr:   "127.0.0.1",
				peer_port:   9000,
			},
			{
				desc:        "an ipv6 peer is recorded without its brackets",
				remote_addr: fakeAddr("[::1]:8080"),
				peer_addr:   "::1",
				peer_port:   8080,
			},
			{
				desc:        "an address with no port is recorded whole",
				remote_addr: fakeAddr("/run/app.sock"),
				peer_addr:   "/run/app.sock",
				peer_port:   -1,
			},
			{
				desc:        "a non numeric port is dropped but the address is kept",
				remote_addr: fakeAddr("127.0.0.1:http"),
				peer_addr:   "127.0.0.1",
				peer_port:   -1,
			},
			{
				desc:        "no address at all is not recorded",
				remote_addr: nil,
				peer_addr:   "",
				peer_port:   -1,
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				x, logs, _ := newOtx(t)
				h := otxgrpc.NewServerLogger(x)

				now := time.Now()
				ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{
					FullMethodName: routeguide.RouteGuide_GetFeature_FullMethodName,
				})
				h.HandleRPC(ctx, &stats.InHeader{RemoteAddr: tc.remote_addr})
				h.HandleRPC(ctx, &stats.Begin{BeginTime: now, IsClientStream: true})
				// A server sends its headers without an address on them; that
				// must not erase what the incoming header established.
				h.HandleRPC(ctx, &stats.OutHeader{})
				h.HandleRPC(ctx, &stats.End{BeginTime: now, EndTime: now})

				records := logs.Records()
				require.Len(t, records, 2)
				for _, r := range records {
					if tc.peer_addr == "" {
						requireNoAttr(t, r, "network.peer.address")
					} else {
						require.Equal(t, tc.peer_addr, attr(t, r, "network.peer.address").AsString())
					}
					if tc.peer_port < 0 {
						requireNoAttr(t, r, "network.peer.port")
					} else {
						require.Equal(t, tc.peer_port, requireInt64Attr(t, r, "network.peer.port"))
					}
				}
			})
		}
	})

	t.Run("a method name that is not /service/method is left off the records", func(t *testing.T) {
		for _, tc := range []struct {
			desc        string
			full_method string
		}{
			{desc: "an empty name", full_method: ""},
			{desc: "a bare slash", full_method: "/"},
			{desc: "a name with no leading slash", full_method: "routeguide.RouteGuide/GetFeature"},
			{desc: "a name with no method", full_method: "/routeguide.RouteGuide/"},
			{desc: "a name with no service", full_method: "/GetFeature"},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				x, logs, _ := newOtx(t)
				h := otxgrpc.NewServerLogger(x)

				// Driven directly: gRPC would not route such a name to a
				// service, but a peer can put anything on the wire.
				now := time.Now()
				ctx := h.TagRPC(context.Background(), &stats.RPCTagInfo{FullMethodName: tc.full_method})
				h.HandleRPC(ctx, &stats.Begin{BeginTime: now, IsClientStream: true})
				h.HandleRPC(ctx, &stats.End{BeginTime: now, EndTime: now})

				records := logs.Records()
				require.Len(t, records, 2, "the records are written anyway")
				for _, r := range records {
					requireNoAttr(t, r, "rpc.service")
					requireNoAttr(t, r, "rpc.method")
				}
			})
		}
	})

	t.Run("the status decides the level of the closing record", func(t *testing.T) {
		for _, tc := range []struct {
			desc  string
			code  codes.Code
			level otellog.Severity
		}{
			{desc: "ok is logged at info", code: codes.OK, level: otellog.SeverityInfo},

			{desc: "unknown is logged at error", code: codes.Unknown, level: otellog.SeverityError},
			{desc: "deadline exceeded is logged at error", code: codes.DeadlineExceeded, level: otellog.SeverityError},
			{desc: "unimplemented is logged at error", code: codes.Unimplemented, level: otellog.SeverityError},
			{desc: "internal is logged at error", code: codes.Internal, level: otellog.SeverityError},
			{desc: "unavailable is logged at error", code: codes.Unavailable, level: otellog.SeverityError},
			{desc: "data loss is logged at error", code: codes.DataLoss, level: otellog.SeverityError},

			{desc: "cancelled is logged at warn", code: codes.Canceled, level: otellog.SeverityWarn},
			{desc: "invalid argument is logged at warn", code: codes.InvalidArgument, level: otellog.SeverityWarn},
			{desc: "not found is logged at warn", code: codes.NotFound, level: otellog.SeverityWarn},
			{desc: "already exists is logged at warn", code: codes.AlreadyExists, level: otellog.SeverityWarn},
			{desc: "permission denied is logged at warn", code: codes.PermissionDenied, level: otellog.SeverityWarn},
			{desc: "resource exhausted is logged at warn", code: codes.ResourceExhausted, level: otellog.SeverityWarn},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				const msg = "the point is off the map"

				service := &routeguide.Server{NumSend: num_res}
				if tc.code != codes.OK {
					service.Err = status.Error(tc.code, msg)
				}

				h := newHarness(t, withService(service))

				err := h.getFeature(t.Context())
				require.Equal(t, tc.code, status.Code(err))
				h.served(t)

				out := h.record(t, msg_out)
				require.Equal(t, tc.level, out.Severity())
				require.EqualValues(t, tc.code, requireInt64Attr(t, out, "rpc.grpc.status_code"))
				require.Equal(t, tc.code.String(), attr(t, out, "rpc.grpc.status").AsString())

				if tc.code == codes.OK {
					// A successful RPC has no status message, so the attribute
					// is left off rather than written empty.
					requireNoAttr(t, out, "rpc.status_message")
				} else {
					require.Equal(t, msg, attr(t, out, "rpc.status_message").AsString())
				}

				// The opening record is always informational; it is written
				// before the status is known.
				in := h.record(t, msg_in)
				require.Equal(t, otellog.SeverityInfo, in.Severity())
				requireNoAttr(t, in, "rpc.grpc.status_code")
				requireNoAttr(t, in, "rpc.status_message")

				// The client sees the same status.
				res := h.record(t, msg_res)
				require.Equal(t, tc.level, res.Severity())
				require.EqualValues(t, tc.code, requireInt64Attr(t, res, "rpc.grpc.status_code"))
			})
		}
	})

	t.Run("a failing rpc of any kind is logged at the level of its status", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t, withService(&routeguide.Server{
					NumSend: num_res,
					Err:     status.Error(codes.Internal, "boom"),
				}))

				err := tc.call(h, t.Context())
				require.Equal(t, codes.Internal, status.Code(err))
				h.served(t)

				out := h.record(t, msg_out)
				require.Equal(t, otellog.SeverityError, out.Severity())
				require.EqualValues(t, codes.Internal, requireInt64Attr(t, out, "rpc.grpc.status_code"))
				require.Equal(t, "boom", attr(t, out, "rpc.status_message").AsString())
			})
		}
	})

	t.Run("the closing record reports the sizes and the counts of the payloads", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				out := h.record(t, msg_out)
				for _, key := range []string{
					"rpc.request.compressed_size",
					"rpc.request.uncompressed_size",
					"rpc.response.compressed_size",
					"rpc.response.uncompressed_size",
				} {
					require.Positivef(t, requireInt64Attr(t, out, key), "want a positive %q", key)
				}

				if !tc.client_stream && !tc.server_stream {
					// A unary RPC carries one message each way, so the counts
					// would say nothing.
					requireNoAttr(t, out, "rpc.request.message_count")
					requireNoAttr(t, out, "rpc.response.message_count")

					return
				}

				require.EqualValues(t, tc.cnt_req, requireInt64Attr(t, out, "rpc.request.message_count"))
				require.EqualValues(t, tc.cnt_res, requireInt64Attr(t, out, "rpc.response.message_count"))
				require.EqualValues(t, tc.cnt_req, h.service.NumRecv(), "the service saw what was reported")
			})
		}
	})

	t.Run("the elapsed time is an int64 on the closing record only", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		out := h.record(t, msg_out)
		require.GreaterOrEqual(t, requireInt64Attr(t, out, "server.elapsed_ns"), int64(0))
		requireNoAttr(t, out, "client.elapsed_ns")

		requireNoAttr(t, h.record(t, msg_in), "server.elapsed_ns")
	})

	t.Run("works on its own, with no tracing handler registered", func(t *testing.T) {
		h := newHarness(t,
			withServerHandlers(func(x *otx.Otx) []stats.Handler {
				return []stats.Handler{otxgrpc.NewServerLogger(x)}
			}),
			withClientHandlers(func(x *otx.Otx) []stats.Handler {
				return nil
			}),
		)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		// It injects the otx itself, so the records reach the exporter...
		require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
		requireMethod(t, h.record(t, msg_in), routeguide.RouteGuide_GetFeature_FullMethodName)

		given, ok := otx.FromOK(h.service.Context())
		require.True(t, ok, "the service implementation still finds the otx")
		require.Same(t, h.otx, given)

		// ...and there is no span for them to take ids from.
		require.Empty(t, h.spans.Ended())
		requireNoSpanIDs(t, h.record(t, msg_in))
		requireNoSpanIDs(t, h.record(t, msg_out))
	})

	t.Run("the closing record of a cancelled rpc still reaches the exporter", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		defer close(release)

		h := newHarness(t, withService(&routeguide.Server{
			NumSend: num_res,
			Entered: entered,
			Release: release,
		}))

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			// The RPC is in flight and the service implementation is holding
			// it open; nothing is released, so it is the cancellation that
			// unblocks it.
			<-entered
			cancel()
		}()

		err := h.getFeature(ctx)
		require.Equal(t, codes.Canceled, status.Code(err))
		h.served(t)

		// The context both records were written with is cancelled by now, which
		// is what would have dropped them at the exporter.
		require.Error(t, h.service.Context().Err())
		require.Equal(t, []string{msg_in, msg_out}, h.serverMessages())
		require.Equal(t, []string{msg_req, msg_res}, h.clientMessages())

		res := h.record(t, msg_res)
		require.Equal(t, otellog.SeverityWarn, res.Severity())
		require.EqualValues(t, codes.Canceled, requireInt64Attr(t, res, "rpc.grpc.status_code"))
	})
}

func TestNewClientLogger(t *testing.T) {
	t.Run("panics on a nil otx", func(t *testing.T) {
		require.PanicsWithValue(t, "otxgrpc: NewClientLogger called with a nil *otx.Otx", func() {
			otxgrpc.NewClientLogger(nil)
		})
	})

	t.Run("writes one record as the rpc is sent and one as it completes", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				require.Equal(t, []string{msg_req, msg_res}, h.clientMessages())
			})
		}
	})

	t.Run("the opening record of every rpc kind comes from stats.Begin", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				// A client writes its opening record before it has sent
				// anything, so no message size is on it whatever the RPC kind.
				req := h.record(t, msg_req)
				requireNoAttr(t, req, "rpc.message.compressed_size")
				requireNoAttr(t, req, "rpc.message.uncompressed_size")
			})
		}
	})

	t.Run("every record names the service and the method and carries the ids of the client span", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				for _, msg := range []string{msg_req, msg_res} {
					r := h.record(t, msg)
					requireMethod(t, r, tc.full_method)
					requireValidSpanIDs(t, r)
				}
			})
		}
	})

	t.Run("the closing record names the peer", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		// The opening record is written before the headers go out, which is
		// where the address of the peer comes from.
		requireNoAttr(t, h.record(t, msg_req), "network.peer.address")
		require.Equal(t, peer_addr, attr(t, h.record(t, msg_res), "network.peer.address").AsString())
	})

	t.Run("the request is what the client sent and the response what it received", func(t *testing.T) {
		for _, tc := range rpcCases() {
			t.Run(tc.desc, func(t *testing.T) {
				h := newHarness(t)

				require.NoError(t, tc.call(h, t.Context()))
				h.served(t)

				res := h.record(t, msg_res)
				out := h.record(t, msg_out)

				// The two sides describe the same RPC from opposite ends, so
				// the request of one is the request of the other. Swapping the
				// direction on either side would break this.
				for _, key := range []string{
					"rpc.request.compressed_size",
					"rpc.request.uncompressed_size",
					"rpc.response.compressed_size",
					"rpc.response.uncompressed_size",
				} {
					v := requireInt64Attr(t, res, key)
					require.Positivef(t, v, "want a positive %q", key)
					require.Equalf(t, requireInt64Attr(t, out, key), v, "the server reports the same %q", key)
				}

				if !tc.client_stream && !tc.server_stream {
					requireNoAttr(t, res, "rpc.request.message_count")
					requireNoAttr(t, res, "rpc.response.message_count")

					return
				}

				require.EqualValues(t, tc.cnt_req, requireInt64Attr(t, res, "rpc.request.message_count"))
				require.EqualValues(t, tc.cnt_res, requireInt64Attr(t, res, "rpc.response.message_count"))
			})
		}
	})

	t.Run("the elapsed time is an int64 on the closing record only", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		res := h.record(t, msg_res)
		require.GreaterOrEqual(t, requireInt64Attr(t, res, "client.elapsed_ns"), int64(0))
		requireNoAttr(t, res, "server.elapsed_ns")

		requireNoAttr(t, h.record(t, msg_req), "client.elapsed_ns")
	})

	t.Run("works on its own, with no tracing handler registered", func(t *testing.T) {
		h := newHarness(t,
			withServerHandlers(func(x *otx.Otx) []stats.Handler {
				return nil
			}),
			withClientHandlers(func(x *otx.Otx) []stats.Handler {
				return []stats.Handler{otxgrpc.NewClientLogger(x)}
			}),
		)

		require.NoError(t, h.getFeature(t.Context()))
		h.served(t)

		require.Equal(t, []string{msg_req, msg_res}, h.clientMessages())
		require.Empty(t, h.spans.Ended())
		requireNoSpanIDs(t, h.record(t, msg_req))
		requireNoSpanIDs(t, h.record(t, msg_res))
	})
}

func TestHandleRPCWithoutTagRPC(t *testing.T) {
	// gRPC always calls HandleRPC with the context TagRPC returned, but a
	// handler that never saw the RPC being tagged - one registered on a server
	// that rejects the RPC first, or a caller driving the type directly - must
	// not bring the process down.
	for _, tc := range []struct {
		desc string
		new  func(x *otx.Otx) stats.Handler
	}{
		{
			desc: "the server logger",
			new: func(x *otx.Otx) stats.Handler {
				return otxgrpc.NewServerLogger(x)
			},
		},
		{
			desc: "the client logger",
			new: func(x *otx.Otx) stats.Handler {
				return otxgrpc.NewClientLogger(x)
			},
		},
		{
			desc: "the server handler",
			new: func(x *otx.Otx) stats.Handler {
				return otxgrpc.NewServerHandler(x)
			},
		},
		{
			desc: "the client handler",
			new: func(x *otx.Otx) stats.Handler {
				return otxgrpc.NewClientHandler(x)
			},
		},
	} {
		t.Run(tc.desc+" ignores a context that never went through TagRPC", func(t *testing.T) {
			x, logs, _ := newOtx(t)
			h := tc.new(x)

			now := time.Now()
			for _, rs := range []stats.RPCStats{
				&stats.Begin{BeginTime: now},
				&stats.InHeader{},
				&stats.OutHeader{},
				&stats.InPayload{Length: 1, CompressedLength: 1},
				&stats.OutPayload{Length: 1, CompressedLength: 1},
				&stats.InTrailer{},
				&stats.OutTrailer{},
				&stats.DelayedPickComplete{},
				&stats.End{BeginTime: now, EndTime: now.Add(time.Second)},
				&stats.End{BeginTime: now, EndTime: now, Error: status.Error(codes.Internal, "boom")},
			} {
				require.NotPanicsf(t, func() {
					h.HandleRPC(context.Background(), rs)
				}, "%T", rs)
			}

			require.NotPanics(t, func() {
				h.HandleConn(context.Background(), &stats.ConnBegin{})
				h.HandleConn(context.Background(), &stats.ConnEnd{})
			})

			// Nothing is written either: there is no RPC to describe.
			require.Empty(t, logs.Records())
		})
	}
}
