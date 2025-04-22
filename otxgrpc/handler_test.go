package otxgrpc_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"github.com/lesomnus/otx/otxgrpc/internal/routeguide"
	"github.com/lesomnus/otx/otxmem"
	"github.com/lesomnus/otx/tag"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestHandler(t *testing.T) {
	require := require.New(t)

	mem_log_exporter := &otxmem.LogExporter{}
	log_provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(mem_log_exporter)),
	)

	my_otx := otx.New(otx.WithLoggerProvider(log_provider))

	service := &routeguide.Server{}
	server := grpc.NewServer(
		grpc.Creds(insecure.NewCredentials()),
		grpc.StatsHandler(otxgrpc.NewServerHandler(my_otx)),
		grpc.StatsHandler(otxgrpc.BoundaryLogger()),
	)
	routeguide.RegisterRouteGuideServer(server, service)

	listener := bufconn.Listen(1 << 20)
	defer listener.Close()

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()
		server.Serve(listener)
	}()
	defer server.GracefulStop()

	conn, err := NewBufConn(listener)
	require.NoError(err)
	defer conn.Close()

	client := routeguide.NewRouteGuideClient(conn)
	_, err = client.GetFeature(t.Context(), nil)
	require.NoError(err)
	require.NotNil(service.Context)
	require.Equal(log_provider, otx.Providers(service.Context).Logger())

	require.Len(mem_log_exporter.Records, 2)

	// Ingress.
	{
		r := mem_log_exporter.Records[0]

		var v *log.Value
		r.WalkAttributes(func(kv log.KeyValue) bool {
			if kv.Key == tag.IngressKey {
				v = &kv.Value
			}
			return true
		})
		require.NotNil(v)

		method := ""
		for _, v := range v.AsMap() {
			if v.Key == "method" {
				require.Equal(log.KindString, v.Value.Kind())
				method = v.Value.AsString()
			}
		}
		require.Equal("/routeguide.RouteGuide/GetFeature", method)
	}

	// Egress.
	{
		r := mem_log_exporter.Records[1]

		var v *log.Value
		r.WalkAttributes(func(kv log.KeyValue) bool {
			if kv.Key == tag.EgressKey {
				v = &kv.Value
			}
			return true
		})
		require.NotNil(v)

		elapsed := time.Duration(0)
		for _, v := range v.AsMap() {
			if v.Key == "elapsed" {
				require.Equal(log.KindInt64, v.Value.Kind())
				elapsed = time.Duration(v.Value.AsInt64())
			}
		}
		require.NotZero(elapsed)
		require.Greater(elapsed, time.Duration(0))
		require.Less(elapsed, time.Second)
	}
}
