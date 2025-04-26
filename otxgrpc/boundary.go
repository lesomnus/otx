package otxgrpc

import (
	"context"
	"log/slog"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/tag"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

var _ stats.Handler = boundaryLogger{}

type boundaryLogger struct{}

func BoundaryLogger() stats.Handler {
	return boundaryLogger{}
}

func (h boundaryLogger) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (h boundaryLogger) HandleRPC(ctx context.Context, rs stats.RPCStats) {
	l := log.From(ctx)
	switch rs := rs.(type) {
	case *stats.InHeader:
		l.Info("in", tag.Ingress(rs.FullMethod))
	case *stats.End:
		dt := rs.EndTime.Sub(rs.BeginTime)
		level := slog.LevelInfo
		attrs := []any{tag.Egress(dt)}
		if rs.Error != nil {
			level = slog.LevelWarn

			code := int(status.Code(rs.Error))
			attrs = append(attrs, slog.Int("code", code))
		}

		ctx := context.WithoutCancel(ctx)
		l.Log(ctx, level, "out", attrs...)
	}
}

func (h boundaryLogger) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (h boundaryLogger) HandleConn(context.Context, stats.ConnStats) {}
