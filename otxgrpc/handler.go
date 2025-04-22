package otxgrpc

import (
	"context"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc/stats"
)

var _ stats.Handler = serverHandler{}

type serverHandler struct {
	stats.Handler
	otx *otx.Otx
}

func NewServerHandler(otx *otx.Otx, opts ...otelgrpc.Option) stats.Handler {
	ps := otx.Providers()
	opts = append([]otelgrpc.Option{
		otelgrpc.WithTracerProvider(ps.Tracer()),
		otelgrpc.WithMeterProvider(ps.Meter()),
	}, opts...)

	return serverHandler{
		Handler: otelgrpc.NewServerHandler(opts...),
		otx:     otx,
	}
}

func (h serverHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	ctx = h.Handler.TagConn(ctx, info)
	ctx = otx.Into(ctx, h.otx)
	return ctx
}
