package otxgrpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxgrpc"
	"github.com/lesomnus/otx/otxgrpc/internal/routeguide"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/test/bufconn"
)

// The number of messages a streaming RPC carries in each direction. They differ
// so that a request count reported as a response count, or the other way round,
// cannot pass.
const (
	num_req = 2 // messages the client sends on a client streaming or bidi RPC
	num_res = 3 // messages the server sends on a server streaming or bidi RPC
)

// The bodies of the four records the two loggers write.
const (
	msg_in  = "gRPC in"
	msg_out = "gRPC out"
	msg_req = "gRPC req"
	msg_res = "gRPC res"
)

// harness is a bufconn server and a client for it, both instrumented with the
// handlers under test, over an *otx.Otx whose logger and tracer providers export
// into memory.
//
// Both processors are synchronous: a record is in the exporter as soon as the
// call that wrote it returns, and a span is in the recorder as soon as it ends.
type harness struct {
	otx   *otx.Otx
	logs  *otxmem.LogExporter
	spans *tracetest.SpanRecorder

	service *routeguide.Server
	client  routeguide.RouteGuideClient

	// The probes registered after the handlers under test on each side.
	server_probe *probe
	client_probe *probe
}

type harnessConf struct {
	service         *routeguide.Server
	server_handlers func(x *otx.Otx) []stats.Handler
	client_handlers func(x *otx.Otx) []stats.Handler
	over_tcp        bool
}

type harnessOpt func(*harnessConf)

// withService replaces the RouteGuide the server serves.
func withService(v *routeguide.Server) harnessOpt {
	return func(c *harnessConf) {
		c.service = v
	}
}

// withServerHandlers replaces the stats handlers registered on the server, in
// registration order.
func withServerHandlers(f func(x *otx.Otx) []stats.Handler) harnessOpt {
	return func(c *harnessConf) {
		c.server_handlers = f
	}
}

// withClientHandlers replaces the stats handlers registered on the client, in
// registration order.
func withClientHandlers(f func(x *otx.Otx) []stats.Handler) harnessOpt {
	return func(c *harnessConf) {
		c.client_handlers = f
	}
}

// overTCP serves over loopback TCP rather than over the in-memory pipe, so that
// the peer of an RPC has an address with a port in it.
func overTCP() harnessOpt {
	return func(c *harnessConf) {
		c.over_tcp = true
	}
}

// newOtx returns an *otx.Otx whose logger and tracer providers export into
// memory, together with the two exporters.
func newOtx(t *testing.T) (*otx.Otx, *otxmem.LogExporter, *tracetest.SpanRecorder) {
	t.Helper()

	logs := &otxmem.LogExporter{}
	spans := tracetest.NewSpanRecorder()
	x := otx.New(
		otx.WithLoggerProvider(sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)),
		)),
		otx.WithTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(spans),
		)),
	)
	t.Cleanup(func() {
		// Not t.Context: it is already cancelled by the time cleanup runs.
		require.NoError(t, x.Shutdown(context.Background()))
	})

	return x, logs, spans
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()

	c := harnessConf{
		service: &routeguide.Server{NumSend: num_res},
		server_handlers: func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{
				otxgrpc.NewServerHandler(x),
				otxgrpc.NewServerLogger(x),
			}
		},
		client_handlers: func(x *otx.Otx) []stats.Handler {
			return []stats.Handler{
				otxgrpc.NewClientHandler(x),
				otxgrpc.NewClientLogger(x),
			}
		},
	}
	for _, f := range opts {
		f(&c)
	}

	x, logs, spans := newOtx(t)

	h := &harness{
		otx:   x,
		logs:  logs,
		spans: spans,

		service: c.service,

		server_probe: newProbe(),
		client_probe: newProbe(),
	}

	server_opts := []grpc.ServerOption{}
	for _, sh := range c.server_handlers(x) {
		server_opts = append(server_opts, grpc.StatsHandler(sh))
	}
	server_opts = append(server_opts, grpc.StatsHandler(h.server_probe))

	server := grpc.NewServer(server_opts...)
	routeguide.RegisterRouteGuideServer(server, c.service)

	var (
		listener net.Listener
		buf      *bufconn.Listener
	)
	if c.over_tcp {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listener = l
	} else {
		buf = bufconn.Listen(1024 * 1024)
		listener = buf
	}

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		<-served
	})

	client_opts := []grpc.DialOption{}
	for _, sh := range c.client_handlers(x) {
		client_opts = append(client_opts, grpc.WithStatsHandler(sh))
	}
	client_opts = append(client_opts, grpc.WithStatsHandler(h.client_probe))

	var (
		conn *grpc.ClientConn
		err  error
	)
	if buf != nil {
		conn, err = NewBufConn(buf, client_opts...)
	} else {
		conn, err = grpc.NewClient(listener.Addr().String(), append([]grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}, client_opts...)...)
	}
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	h.client = routeguide.NewRouteGuideClient(conn)

	return h
}

// probe is the stats handler the harness registers after the handlers under
// test. gRPC calls stats handlers in registration order, so by the time this one
// sees an event the handlers under test have already handled it: its context is
// the one they built, and its End signal means their records are exported.
type probe struct {
	done chan struct{}

	mu  sync.Mutex
	ctx context.Context
}

var _ stats.Handler = (*probe)(nil)

func newProbe() *probe {
	// Buffered so that an RPC is never blocked by a test that does not wait for
	// it; no test in this package makes more calls than this.
	return &probe{done: make(chan struct{}, 64)}
}

func (p *probe) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctx = ctx

	return ctx
}

func (p *probe) HandleRPC(_ context.Context, rs stats.RPCStats) {
	if _, ok := rs.(*stats.End); ok {
		p.done <- struct{}{}
	}
}

func (p *probe) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (p *probe) HandleConn(context.Context, stats.ConnStats) {}

// Context returns the RPC context the handlers under test built.
func (p *probe) Context() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.ctx
}

// wait blocks until one more RPC has run through the handlers under test. The
// deadline is a guard against a hang, not a synchronisation device.
func (p *probe) wait(t *testing.T) {
	t.Helper()

	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for an RPC to finish")
	}
}

// served blocks until the server side of one more RPC is over, records
// included. The client side is synchronous - gRPC writes the closing stats event
// of an RPC before the call that made it returns - so there is no counterpart
// for it.
func (h *harness) served(t *testing.T) {
	t.Helper()
	h.server_probe.wait(t)
}

// getFeature makes the unary RPC.
func (h *harness) getFeature(ctx context.Context) error {
	_, err := h.client.GetFeature(ctx, &routeguide.Point{Latitude: 1, Longitude: 2})

	return err
}

// listFeatures makes the server streaming RPC and drains it, so that the whole
// response has been seen by the time it returns.
func (h *harness) listFeatures(ctx context.Context) error {
	stream, err := h.client.ListFeatures(ctx, &routeguide.Rectangle{
		Lo: &routeguide.Point{Latitude: 1, Longitude: 2},
		Hi: &routeguide.Point{Latitude: 3, Longitude: 4},
	})
	if err != nil {
		return err
	}

	return drain(stream)
}

// recordRoute makes the client streaming RPC, sending num_req points.
func (h *harness) recordRoute(ctx context.Context) error {
	stream, err := h.client.RecordRoute(ctx)
	if err != nil {
		return err
	}
	for i := range num_req {
		p := &routeguide.Point{Latitude: int32(i + 1), Longitude: int32(i + 1)}
		if err := stream.Send(p); err != nil {
			// Send reports a broken stream as io.EOF; the status is on the
			// closing call.
			break
		}
	}

	_, err = stream.CloseAndRecv()

	return err
}

// routeChat makes the bidi RPC, sending num_req notes and then draining the
// response.
func (h *harness) routeChat(ctx context.Context) error {
	stream, err := h.client.RouteChat(ctx)
	if err != nil {
		return err
	}
	for i := range num_req {
		n := &routeguide.RouteNote{
			Location: &routeguide.Point{Latitude: int32(i + 1), Longitude: int32(i + 1)},
			Message:  fmt.Sprintf("note-%d", i),
		}
		if err := stream.Send(n); err != nil {
			break
		}
	}
	if err := stream.CloseSend(); err != nil {
		return err
	}

	return drain(stream)
}

// rpcCase drives one of the four RPC kinds and says what it puts on the wire.
type rpcCase struct {
	desc        string
	full_method string
	call        func(h *harness, ctx context.Context) error

	client_stream bool
	server_stream bool

	// The messages a successful call carries, seen from the client: cnt_req is
	// what it sends and cnt_res what it receives.
	cnt_req int
	cnt_res int
}

func rpcCases() []rpcCase {
	return []rpcCase{
		{
			desc:        "unary",
			full_method: routeguide.RouteGuide_GetFeature_FullMethodName,
			call:        (*harness).getFeature,
			cnt_req:     1,
			cnt_res:     1,
		},
		{
			desc:          "server streaming",
			full_method:   routeguide.RouteGuide_ListFeatures_FullMethodName,
			call:          (*harness).listFeatures,
			server_stream: true,
			cnt_req:       1,
			cnt_res:       num_res,
		},
		{
			desc:          "client streaming",
			full_method:   routeguide.RouteGuide_RecordRoute_FullMethodName,
			call:          (*harness).recordRoute,
			client_stream: true,
			cnt_req:       num_req,
			cnt_res:       1,
		},
		{
			desc:          "bidi streaming",
			full_method:   routeguide.RouteGuide_RouteChat_FullMethodName,
			call:          (*harness).routeChat,
			client_stream: true,
			server_stream: true,
			cnt_req:       num_req,
			cnt_res:       num_res,
		},
	}
}

type receiver[T any] interface {
	Recv() (T, error)
}

// drain reads a stream to its end, so that the client has observed the status
// of the RPC by the time it returns.
func drain[T any](stream receiver[T]) error {
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// records returns every record exported so far, in order.
func (h *harness) records() []sdklog.Record {
	return h.logs.Records()
}

// messages returns the body of every record exported so far, in order.
func (h *harness) messages() []string {
	rs := h.records()
	vs := make([]string, len(rs))
	for i := range rs {
		vs[i] = rs[i].Body().AsString()
	}

	return vs
}

// serverMessages returns the bodies of the records the server logger wrote, in
// order. The two sides run on different goroutines, so only the order within one
// side is meaningful.
func (h *harness) serverMessages() []string {
	return filterMessages(h.messages(), msg_in, msg_out)
}

// clientMessages returns the bodies of the records the client logger wrote, in
// order.
func (h *harness) clientMessages() []string {
	return filterMessages(h.messages(), msg_req, msg_res)
}

func filterMessages(msgs []string, keep ...string) []string {
	vs := []string{}
	for _, msg := range msgs {
		for _, k := range keep {
			if msg == k {
				vs = append(vs, msg)
				break
			}
		}
	}

	return vs
}

// record returns the one record whose body is msg, failing if there is none or
// more than one.
func (h *harness) record(t *testing.T, msg string) sdklog.Record {
	t.Helper()

	var (
		found sdklog.Record
		n     int
	)
	for _, r := range h.records() {
		if r.Body().AsString() != msg {
			continue
		}

		found = r
		n++
	}

	require.Equalf(t, 1, n, "want exactly one %q record, got %d of them in %v", msg, n, h.messages())
	return found
}

// attrCount reports how many attributes of r carry the given key.
func attrCount(r sdklog.Record, key string) int {
	n := 0
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Key == key {
			n++
		}

		return true
	})

	return n
}

// attr returns the value of the single attribute of r with the given key.
func attr(t *testing.T, r sdklog.Record, key string) otellog.Value {
	t.Helper()

	var v otellog.Value
	require.Equalf(t, 1, attrCount(r, key), "want exactly one %q attribute in %q, got %d", key, r.Body().AsString(), attrCount(r, key))
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Key != key {
			return true
		}

		v = kv.Value
		return false
	})

	return v
}

// requireNoAttr fails if r carries an attribute with the given key.
func requireNoAttr(t *testing.T, r sdklog.Record, key string) {
	t.Helper()
	require.Zerof(t, attrCount(r, key), "want no %q attribute in %q", key, r.Body().AsString())
}

// requireInt64Attr asserts that the attribute is stored as an int64, not as a
// float or a string, and returns it.
func requireInt64Attr(t *testing.T, r sdklog.Record, key string) int64 {
	t.Helper()

	v := attr(t, r, key)
	require.Equalf(t, otellog.KindInt64, v.Kind(), "want %q to be an int64 attribute", key)

	return v.AsInt64()
}

// requireMethod asserts that the record names the RPC, with the service and the
// method split out of the full method name.
func requireMethod(t *testing.T, r sdklog.Record, full_method string) {
	t.Helper()

	i := strings.LastIndex(full_method, "/")
	require.Positive(t, i, "a full method name is /service/method")

	require.Equal(t, full_method[1:i], attr(t, r, "rpc.service").AsString())
	require.Equal(t, full_method[i+1:], attr(t, r, "rpc.method").AsString())
}

// requireSpanIDs asserts that the record carries the ids of span, both in the
// dedicated record fields the SDK fills in and in the attributes the log package
// adds for plain slog handlers.
func requireSpanIDs(t *testing.T, r sdklog.Record, span trace.SpanContext) {
	t.Helper()

	require.Equal(t, span.TraceID(), r.TraceID())
	require.Equal(t, span.SpanID(), r.SpanID())
	require.Equal(t, span.TraceID().String(), attr(t, r, "trace_id").AsString())
	require.Equal(t, span.SpanID().String(), attr(t, r, "span_id").AsString())
}

// requireValidSpanIDs asserts that the record was written with a span active,
// which is what shows it reached the exporter through the span carrying context
// the tracing handler built.
func requireValidSpanIDs(t *testing.T, r sdklog.Record) {
	t.Helper()

	require.Truef(t, r.TraceID().IsValid(), "want a trace id on %q", r.Body().AsString())
	require.Truef(t, r.SpanID().IsValid(), "want a span id on %q", r.Body().AsString())
}

// requireNoSpanIDs asserts that the record was written with no span active.
func requireNoSpanIDs(t *testing.T, r sdklog.Record) {
	t.Helper()

	require.Falsef(t, r.TraceID().IsValid(), "want no trace id on %q", r.Body().AsString())
	require.Falsef(t, r.SpanID().IsValid(), "want no span id on %q", r.Body().AsString())
	requireNoAttr(t, r, "trace_id")
	requireNoAttr(t, r, "span_id")
}

// span returns the one ended span of the given kind, failing if there is none or
// more than one.
func (h *harness) span(t *testing.T, kind trace.SpanKind) sdktrace.ReadOnlySpan {
	t.Helper()

	var (
		found sdktrace.ReadOnlySpan
		n     int
	)
	for _, s := range h.spans.Ended() {
		if s.SpanKind() != kind {
			continue
		}

		found = s
		n++
	}

	require.Equalf(t, 1, n, "want exactly one %v span, got %d", kind, n)
	return found
}
