package otxgrpc

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lesomnus/otx/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.38.0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

var _ stats.Handler = serverLogger{}

type serverLogger struct{}

func NewServerLogger() stats.Handler {
	return serverLogger{}
}

type ctxKey struct{}

type logCtx struct {
	*slog.Logger
	t0 time.Time

	client_stream bool
	server_stream bool
	remote_addr   string

	cnt_in  atomic.Int64
	cnt_out atomic.Int64

	size_recv  atomic.Int64
	size_read  atomic.Int64
	size_write atomic.Int64
	size_sent  atomic.Int64
}

func (h serverLogger) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	l := log.From(ctx)
	if i := strings.LastIndex(info.FullMethodName, "/"); i >= 0 {
		l = l.With(
			slog.String(string(semconv.RPCServiceKey), info.FullMethodName[1:i]),
			slog.String(string(semconv.RPCMethodKey), info.FullMethodName[i+1:]),
		)
	}

	lc := &logCtx{Logger: l, t0: time.Now()}
	return context.WithValue(ctx, ctxKey{}, lc)
}

func (h serverLogger) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	l := ctx.Value(ctxKey{}).(*logCtx)

	switch rs := rs.(type) {
	case *stats.Begin:
		l.t0 = rs.BeginTime
		l.client_stream = rs.IsClientStream
		l.server_stream = rs.IsServerStream
		if l.client_stream {
			l.Info("gRPC in",
				slog.String(string(semconv.NetworkPeerAddressKey), l.remote_addr),
			)
		}

	case *stats.InHeader:
		l.remote_addr = rs.RemoteAddr.String()
		if i := strings.LastIndexByte(l.remote_addr, ':'); i > 0 {
			l.remote_addr = l.remote_addr[:i]
		}

	case *stats.InPayload:
		l.cnt_in.Add(1)
		l.size_recv.Add(int64(rs.CompressedLength))
		l.size_read.Add(int64(rs.Length))
		if !l.client_stream {
			l.Info("gRPC in",
				slog.Int(string(semconv.RPCMessageCompressedSizeKey), int(l.size_recv.Load())),
				slog.Int(string(semconv.RPCMessageUncompressedSizeKey), int(l.size_read.Load())),
				slog.String(string(semconv.NetworkPeerAddressKey), l.remote_addr),
			)
		}

	case *stats.OutPayload:
		l.cnt_out.Add(1)
		l.size_sent.Add(int64(rs.CompressedLength))
		l.size_write.Add(int64(rs.Length))

	case *stats.OutHeader:
	case *stats.OutTrailer:

	case *stats.End:
		dt := rs.EndTime.Sub(rs.BeginTime)

		level := slog.LevelWarn
		code := status.Code(rs.Error)
		switch code {
		case codes.OK:
			level = slog.LevelInfo
		case codes.Internal:
			level = slog.LevelError
		}

		attrs := []any{
			slog.Int(string(semconv.RPCGRPCStatusCodeKey), int(status.Code(rs.Error))),
			slog.Int64("server.elapsed_ns", dt.Nanoseconds()),
		}
		if l.client_stream {
			// Client streaming or bidi
			attrs = append(attrs,
				slog.Int("rpc.request.compressed_size", int(l.size_recv.Load())),
				slog.Int("rpc.request.uncompressed_size", int(l.size_read.Load())),
				slog.Int("rpc.response.compressed_size", int(l.size_sent.Load())),
				slog.Int("rpc.response.uncompressed_size", int(l.size_write.Load())),
			)
		} else {
			// Unary or server streaming
			attrs = append(attrs,
				slog.Int(string(semconv.RPCMessageCompressedSizeKey), int(l.size_sent.Load())),
				slog.Int(string(semconv.RPCMessageUncompressedSizeKey), int(l.size_write.Load())),
			)
		}

		ctx := context.WithoutCancel(ctx)
		l.Log(ctx, level, "gRPC out", attrs...)
	}
}

func (h serverLogger) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h serverLogger) HandleConn(context.Context, stats.ConnStats) {}
