// Package otxgrpc carries an [github.com/lesomnus/otx.Otx] across a gRPC
// boundary as a [google.golang.org/grpc/stats.Handler].
//
// [NewServerHandler] and [NewClientHandler] wrap the corresponding
// [go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc]
// handler with the providers and propagator held by the Otx, and put the Otx
// into the RPC context so that [github.com/lesomnus/otx/log.From] resolves in
// the service implementation.
//
// [NewServerLogger] and [NewClientLogger] are separate handlers that write one
// record per RPC boundary. gRPC accepts several stats handlers and calls them
// in registration order, so register the tracing handler first for the log
// records to carry the ids of its span:
//
//	grpc.NewServer(
//		grpc.StatsHandler(otxgrpc.NewServerHandler(x)),
//		grpc.StatsHandler(otxgrpc.NewServerLogger(x)),
//	)
package otxgrpc

import (
	"context"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc/stats"
)

var _ stats.Handler = handler{}

type handler struct {
	stats.Handler
	otx *otx.Otx
}

// NewServerHandler returns a [google.golang.org/grpc/stats.Handler] that
// traces each RPC and puts x into its context.
//
// The tracer provider, meter provider and propagator of x are given to
// otelgrpc; anything in opts is applied after them and so wins.
//
// It panics if x is nil, at wiring time rather than on the first RPC.
func NewServerHandler(x *otx.Otx, opts ...otelgrpc.Option) stats.Handler {
	if x == nil {
		panic("otxgrpc: NewServerHandler called with a nil *otx.Otx")
	}

	return handler{
		Handler: otelgrpc.NewServerHandler(withOtx(x, opts)...),
		otx:     x,
	}
}

// NewClientHandler returns a [google.golang.org/grpc/stats.Handler] that
// traces each outgoing RPC and puts x into its context, so that the records
// written by [NewClientLogger] and by the calling code reach the providers x
// holds even when the call was made from a bare context.
//
// It panics if x is nil, at wiring time rather than on the first RPC.
func NewClientHandler(x *otx.Otx, opts ...otelgrpc.Option) stats.Handler {
	if x == nil {
		panic("otxgrpc: NewClientHandler called with a nil *otx.Otx")
	}

	return handler{
		Handler: otelgrpc.NewClientHandler(withOtx(x, opts)...),
		otx:     x,
	}
}

func withOtx(x *otx.Otx, opts []otelgrpc.Option) []otelgrpc.Option {
	ps := x.Providers()

	return append([]otelgrpc.Option{
		otelgrpc.WithTracerProvider(ps.Tracer()),
		otelgrpc.WithMeterProvider(ps.Meter()),
		otelgrpc.WithPropagators(x.Propagator()),
	}, opts...)
}

// TagConn puts the Otx into the connection context, which gRPC uses as the
// parent of every RPC context on that connection. That makes the Otx reachable
// from a stats handler registered before this one, whose TagRPC would
// otherwise run first.
func (h handler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	ctx = h.Handler.TagConn(ctx, info)
	ctx = otx.Into(ctx, h.otx)

	return ctx
}

// TagRPC puts the Otx into the RPC context. On the client side there is no
// connection context to inherit from, so this is the only injection point.
func (h handler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	ctx = h.Handler.TagRPC(ctx, info)
	ctx = otx.Into(ctx, h.otx)

	return ctx
}
