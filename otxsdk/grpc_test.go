package otxsdk_test

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxsdk"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracespb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
)

// Everything else in this module reaches a collector over HTTP. The gRPC
// protocol is a different exporter for every signal, and "New returned an SDK
// provider" is the same observation whichever of the two was built, so an
// otlptracegrpc that was constructed and then never shipped anything would
// look identical. This stands up a real gRPC collector and asks it what it
// received.

// grpcCollector is an OTLP/gRPC endpoint that accepts every signal.
type grpcCollector struct {
	addr string

	mu      sync.Mutex
	traces  []*coltracespb.ExportTraceServiceRequest
	logs    []*collogspb.ExportLogsServiceRequest
	metrics []*colmetricspb.ExportMetricsServiceRequest
}

// The three services are separate types because all three Export methods
// would otherwise collide on one struct.
type (
	traceService struct {
		coltracespb.UnimplementedTraceServiceServer
		c *grpcCollector
	}
	logsService struct {
		collogspb.UnimplementedLogsServiceServer
		c *grpcCollector
	}
	metricsService struct {
		colmetricspb.UnimplementedMetricsServiceServer
		c *grpcCollector
	}
)

func (s traceService) Export(ctx context.Context, req *coltracespb.ExportTraceServiceRequest) (*coltracespb.ExportTraceServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()

	s.c.traces = append(s.c.traces, req)

	return &coltracespb.ExportTraceServiceResponse{}, nil
}

func (s logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()

	s.c.logs = append(s.c.logs, req)

	return &collogspb.ExportLogsServiceResponse{}, nil
}

func (s metricsService) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()

	s.c.metrics = append(s.c.metrics, req)

	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// newGRPCCollector starts a collector and points OTEL_EXPORTER_OTLP_ENDPOINT
// at it. The http scheme is what tells the exporters to talk plaintext.
//
// An export is recorded before its response is written, so an export that has
// been acknowledged - which is what ForceFlush waits for - has already been
// recorded. There is nothing to wait for afterwards.
func newGRPCCollector(t *testing.T) *grpcCollector {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	c := &grpcCollector{addr: lis.Addr().String()}

	srv := grpc.NewServer()
	coltracespb.RegisterTraceServiceServer(srv, traceService{c: c})
	collogspb.RegisterLogsServiceServer(srv, logsService{c: c})
	colmetricspb.RegisterMetricsServiceServer(srv, metricsService{c: c})

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-served
	})

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.endpoint())

	return c
}

// endpoint is the value OTEL_EXPORTER_OTLP_ENDPOINT and its per signal
// variants take to reach this collector.
func (c *grpcCollector) endpoint() string {
	return "http://" + c.addr
}

// spanNames returns the name of every span received so far.
func (c *grpcCollector) spanNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	names := []string{}
	for _, req := range c.traces {
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					names = append(names, s.GetName())
				}
			}
		}
	}

	return names
}

// logBodies returns the body of every log record received so far.
func (c *grpcCollector) logBodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	bodies := []string{}
	for _, req := range c.logs {
		for _, rl := range req.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				for _, r := range sl.GetLogRecords() {
					bodies = append(bodies, r.GetBody().GetStringValue())
				}
			}
		}
	}

	return bodies
}

// metricNames returns the name of every metric received so far.
func (c *grpcCollector) metricNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	names := []string{}
	for _, req := range c.metrics {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					names = append(names, m.GetName())
				}
			}
		}
	}

	return names
}

// resourceAttrs returns the resource the first trace export arrived with.
func (c *grpcCollector) resourceAttrs(t *testing.T) []*commonpb.KeyValue {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	require.NotEmpty(t, c.traces, "no trace export")
	require.NotEmpty(t, c.traces[0].GetResourceSpans(), "no resource spans")

	return c.traces[0].GetResourceSpans()[0].GetResource().GetAttributes()
}

func TestNewGRPC(t *testing.T) {
	t.Run("all three signals reach a real collector over grpc", func(t *testing.T) {
		clearEnv(t)
		g := newGRPCCollector(t)
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		t.Setenv("OTEL_SERVICE_NAME", "checkout")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		counter, err := x.Meter().Int64Counter("requests")
		require.NoError(t, err)
		counter.Add(t.Context(), 1)

		ctx, span := x.TraceStart(t.Context(), "op")
		slog.New(x.SlogHandler()).InfoContext(ctx, "hello")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(context.Background()))

		require.Equal(t, []string{"op"}, g.spanNames())
		require.Equal(t, []string{"hello"}, g.logBodies())
		require.Contains(t, g.metricNames(), "requests")

		requireAttr(t, g.resourceAttrs(t), "service.name", "checkout")
	})
	t.Run("a per signal protocol sends that signal over its own transport", func(t *testing.T) {
		// Traces go to the gRPC collector and logs to the HTTP one, each named
		// by its own endpoint variable, so the two are told apart by where
		// they arrive rather than by the type of the provider.
		clearEnv(t)
		g := newGRPCCollector(t)
		c := newCollector(t)
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", g.endpoint())
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "http/protobuf")
		// A per signal HTTP endpoint is used as given, with no path appended.
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", c.url+"/v1/logs")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		ctx, span := x.TraceStart(t.Context(), "op")
		slog.New(x.SlogHandler()).InfoContext(ctx, "hello")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(context.Background()))

		require.Equal(t, []string{"op"}, g.spanNames())
		require.Empty(t, g.logBodies())

		require.Equal(t, []string{"/v1/logs"}, c.paths())
		_, _, record := onlyRecord(t, logsOf(t, c.waitFor(t, "/v1/logs")))
		require.Equal(t, "hello", record.GetBody().GetStringValue())
	})
	t.Run("the scope and the resource reach a grpc exporter too", func(t *testing.T) {
		clearEnv(t)
		g := newGRPCCollector(t)
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x, err := otxsdk.New(t.Context(),
			otxsdk.WithResourceAttributes(attribute.String("region", "eu-west-1")),
			otxsdk.WithOtxOptions(otx.WithScopeName("app")),
		)
		require.NoError(t, err)

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(context.Background()))

		require.Equal(t, []string{"op"}, g.spanNames())
		requireAttr(t, g.resourceAttrs(t), "region", "eu-west-1")

		g.mu.Lock()
		defer g.mu.Unlock()
		require.Equal(t, "app", g.traces[0].GetResourceSpans()[0].GetScopeSpans()[0].GetScope().GetName())
	})
}
