package otxgrpc_test

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func NewBufConn(listener *bufconn.Listener, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough://bufnet", append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	}, opts...)...)
}
