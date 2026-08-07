package otxgrpc

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	// v1.38.0 rather than the v1.40.0 the rest of the repository uses: it is
	// the last version that still defines rpc.service, rpc.grpc.status_code
	// and the rpc.message.* sizes, which the records below record.
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

var _ stats.Handler = rpcLogger{}

// Attribute keys for the payload sizes of a whole RPC, which the per-message
// keys of semconv do not cover.
const (
	requestCompressedSizeKey    = "rpc.request.compressed_size"
	requestUncompressedSizeKey  = "rpc.request.uncompressed_size"
	responseCompressedSizeKey   = "rpc.response.compressed_size"
	responseUncompressedSizeKey = "rpc.response.uncompressed_size"
)

// NewServerLogger returns a [google.golang.org/grpc/stats.Handler] that writes
// one record as an RPC arrives ("gRPC in") and one as it completes ("gRPC
// out"), the latter at a level derived from the status code.
//
// It puts x into the RPC context itself, so it works on its own. Registering
// it after [NewServerHandler] is still preferable: the server span exists by
// then, so the records carry its ids.
//
// See [WithFilter] for leaving out the RPCs that are polled rather than called.
//
// It panics if x is nil, at wiring time rather than on the first RPC.
func NewServerLogger(x *otx.Otx, opts ...Option) stats.Handler {
	if x == nil {
		panic("otxgrpc: NewServerLogger called with a nil *otx.Otx")
	}

	return newRPCLogger(x, false, opts)
}

// NewClientLogger returns a [google.golang.org/grpc/stats.Handler] that writes
// one record as an outgoing RPC is sent ("gRPC req") and one as it completes
// ("gRPC res").
//
// It panics if x is nil, at wiring time rather than on the first RPC.
func NewClientLogger(x *otx.Otx, opts ...Option) stats.Handler {
	if x == nil {
		panic("otxgrpc: NewClientLogger called with a nil *otx.Otx")
	}

	return newRPCLogger(x, true, opts)
}

func newRPCLogger(x *otx.Otx, is_client bool, opts []Option) rpcLogger {
	l := rpcLogger{otx: x, is_client: is_client}
	for _, o := range opts {
		o.apply(&l)
	}

	return l
}

type rpcLogger struct {
	otx       *otx.Otx
	is_client bool

	// filter is asked of every RPC, and nothing is written for one it declines.
	// Nil is everything.
	filter Filter
}

type logCtxKey struct{}

// logCtx accumulates what is known about one RPC.
//
// gRPC re-enters HandleRPC concurrently for a single RPC: on a bidirectional
// stream the InPayload and OutPayload events come from whichever goroutines
// the service implementation reads and writes on. Only the counters are
// touched there, and they are atomic. The plain fields below are written from
// the Begin and header events, which all run on the stream goroutine before
// the service implementation is handed the stream.
type logCtx struct {
	client_stream bool
	server_stream bool
	remote_addr   string
	remote_port   int

	cnt_in  atomic.Int64
	cnt_out atomic.Int64

	size_recv  atomic.Int64
	size_read  atomic.Int64
	size_write atomic.Int64
	size_sent  atomic.Int64
}

func (h rpcLogger) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = otx.Into(ctx, h.otx)

	// The service and method are stashed on a logger rather than held on the
	// logCtx so that every record picks them up, while the span ids are still
	// resolved from the context each record is emitted with. That keeps the
	// records correlated even when this handler is registered before the one
	// that starts the span.
	l := log.From(ctx)
	// A well formed name is "/package.Service/Method". A peer can send
	// anything, so the split is bounds checked rather than assumed: "/" alone
	// would otherwise slice [1:0].
	if service, method, ok := splitMethod(info.FullMethodName); ok {
		l = l.With(
			slog.String(string(semconv.RPCServiceKey), service),
			slog.String(string(semconv.RPCMethodKey), method),
		)
	}
	ctx = log.Into(ctx, l)

	if f := h.filter; f != nil && !f(info) {
		// Nothing is stashed, and HandleRPC writes nothing it cannot find. What
		// the RPC itself is served with is left as it was: a filter says which
		// records this handler writes, not what the call can reach.
		return ctx
	}

	return context.WithValue(ctx, logCtxKey{}, &logCtx{})
}

func (h rpcLogger) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	// gRPC calls HandleRPC with the context TagRPC returned, but a handler
	// registered on a server that rejects an RPC before tagging it, or a
	// caller driving this type directly, would not. Missing state is not worth
	// a panic in a request path.
	lc, ok := ctx.Value(logCtxKey{}).(*logCtx)
	if !ok {
		return
	}

	switch rs := rs.(type) {
	case *stats.Begin:
		lc.client_stream = rs.IsClientStream
		lc.server_stream = rs.IsServerStream
		if h.is_client || lc.client_stream {
			// A client-streaming or bidi RPC has no single request payload to
			// wait for, so the opening record is written here. On the server,
			// InHeader has already run and the peer address is known.
			log.From(ctx).Info(h.msgOpen(), lc.peerAttrs()...)
		}

	case *stats.InHeader:
		lc.setPeer(rs.RemoteAddr)

	case *stats.OutHeader:
		// The peer of an outgoing call. On a server this event carries no
		// address, so setPeer keeps the one InHeader already established.
		lc.setPeer(rs.RemoteAddr)

	case *stats.InPayload:
		lc.cnt_in.Add(1)
		lc.size_recv.Add(int64(rs.CompressedLength))
		lc.size_read.Add(int64(rs.Length))
		if !h.is_client && !lc.client_stream {
			// Unary or server streaming: the single request payload is the
			// natural place to open the record.
			log.From(ctx).Info(h.msgOpen(), append(lc.peerAttrs(),
				slog.Int64(string(semconv.RPCMessageCompressedSizeKey), lc.size_recv.Load()),
				slog.Int64(string(semconv.RPCMessageUncompressedSizeKey), lc.size_read.Load()),
			)...)
		}

	case *stats.OutPayload:
		lc.cnt_out.Add(1)
		lc.size_sent.Add(int64(rs.CompressedLength))
		lc.size_write.Add(int64(rs.Length))

	case *stats.End:
		dt := rs.EndTime.Sub(rs.BeginTime)

		st, _ := status.FromError(rs.Error)
		code := st.Code()

		attrs := append(lc.peerAttrs(),
			slog.Int(string(semconv.RPCGRPCStatusCodeKey), int(code)),
			slog.String("rpc.grpc.status", code.String()),
			slog.Int64(h.elapsedKey(), dt.Nanoseconds()),
		)
		if msg := st.Message(); msg != "" {
			attrs = append(attrs, slog.String("rpc.status_message", msg))
		}
		attrs = append(attrs, lc.sizeAttrs(h.is_client)...)

		// Detached from cancellation. This is load bearing for every RPC,
		// not only cancelled ones: gRPC cancels the stream context as soon
		// as the RPC completes, which is before the deferred End event runs,
		// so an exporter that honours ctx.Err would drop every closing
		// record.
		ctx := context.WithoutCancel(ctx)
		log.From(ctx).Log(ctx, levelOf(code), h.msgClose(), attrs...)
	}
}

func (h rpcLogger) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return otx.Into(ctx, h.otx)
}

func (h rpcLogger) HandleConn(context.Context, stats.ConnStats) {}

func (h rpcLogger) msgOpen() string {
	if h.is_client {
		return "gRPC req"
	}

	return "gRPC in"
}

func (h rpcLogger) msgClose() string {
	if h.is_client {
		return "gRPC res"
	}

	return "gRPC out"
}

func (h rpcLogger) elapsedKey() string {
	if h.is_client {
		return "client.elapsed_ns"
	}

	return "server.elapsed_ns"
}

// setPeer records the address of the other end, ignoring an empty one so that
// a later event without an address cannot erase what an earlier one knew.
func (c *logCtx) setPeer(addr net.Addr) {
	host, port := splitAddr(addr)
	if host == "" {
		return
	}

	c.remote_addr, c.remote_port = host, port
}

func (c *logCtx) peerAttrs() []any {
	attrs := make([]any, 0, 8)
	if c.remote_addr != "" {
		attrs = append(attrs, slog.String(string(semconv.NetworkPeerAddressKey), c.remote_addr))
	}
	if c.remote_port > 0 {
		attrs = append(attrs, slog.Int(string(semconv.NetworkPeerPortKey), c.remote_port))
	}

	return attrs
}

// sizeAttrs reports the payload sizes of a finished RPC. A stream carries many
// messages, so its totals are reported under their own keys rather than under
// the per-message keys of semconv, and the message counts are reported too.
func (c *logCtx) sizeAttrs(is_client bool) []any {
	req_compressed, req_uncompressed := c.size_recv.Load(), c.size_read.Load()
	res_compressed, res_uncompressed := c.size_sent.Load(), c.size_write.Load()
	if is_client {
		req_compressed, res_compressed = res_compressed, req_compressed
		req_uncompressed, res_uncompressed = res_uncompressed, req_uncompressed
	}

	if !c.client_stream && !c.server_stream {
		// Unary: one message each way, so the per-message keys are exact.
		return []any{
			slog.Int64(requestCompressedSizeKey, req_compressed),
			slog.Int64(requestUncompressedSizeKey, req_uncompressed),
			slog.Int64(responseCompressedSizeKey, res_compressed),
			slog.Int64(responseUncompressedSizeKey, res_uncompressed),
		}
	}

	cnt_req, cnt_res := c.cnt_in.Load(), c.cnt_out.Load()
	if is_client {
		cnt_req, cnt_res = cnt_res, cnt_req
	}

	return []any{
		slog.Int64(requestCompressedSizeKey, req_compressed),
		slog.Int64(requestUncompressedSizeKey, req_uncompressed),
		slog.Int64(responseCompressedSizeKey, res_compressed),
		slog.Int64(responseUncompressedSizeKey, res_uncompressed),
		slog.Int64("rpc.request.message_count", cnt_req),
		slog.Int64("rpc.response.message_count", cnt_res),
	}
}

// levelOf maps a gRPC status to a log level: a code that means the server
// failed is an error, one that means the caller was refused is a warning.
func levelOf(code codes.Code) slog.Level {
	switch code {
	case codes.OK:
		return slog.LevelInfo

	case codes.Unknown,
		codes.DeadlineExceeded,
		codes.Unimplemented,
		codes.Internal,
		codes.Unavailable,
		codes.DataLoss:
		return slog.LevelError

	default:
		return slog.LevelWarn
	}
}

// splitMethod separates the service and method of a fully qualified gRPC
// method name, reporting false for a name that is not of the expected shape.
func splitMethod(name string) (string, string, bool) {
	name, ok := strings.CutPrefix(name, "/")
	if !ok {
		return "", "", false
	}

	// LastIndex, matching how gRPC itself splits the name.
	i := strings.LastIndex(name, "/")
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}

	return name[:i], name[i+1:], true
}

// splitAddr separates the host and port of a peer address. The host of an IPv6
// address is returned without its brackets, which is what the semantic
// convention asks for.
func splitAddr(addr net.Addr) (string, int) {
	if addr == nil {
		return "", 0
	}

	s := addr.String()
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return s, 0
	}

	n, err := strconv.Atoi(port)
	if err != nil {
		return host, 0
	}

	return host, n
}
