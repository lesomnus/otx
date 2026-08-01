package otxhttp_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/metric"
	metricembedded "go.opentelemetry.io/otel/metric/embedded"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// harness is an *otx.Otx whose logger and tracer providers export into memory,
// so that the exact records and spans a request produces can be asserted on.
//
// Both processors are synchronous: a record is in the exporter as soon as the
// call that wrote it returns, and a span is in the recorder as soon as it ends.
type harness struct {
	otx   *otx.Otx
	logs  *otxmem.LogExporter
	spans *tracetest.SpanRecorder
}

func newHarness(t *testing.T, opts ...otx.Option) *harness {
	t.Helper()

	logs := &otxmem.LogExporter{}
	logger_provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)),
	)

	spans := tracetest.NewSpanRecorder()
	tracer_provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spans),
	)

	x := otx.New(append([]otx.Option{
		otx.WithLoggerProvider(logger_provider),
		otx.WithTracerProvider(tracer_provider),
	}, opts...)...)
	t.Cleanup(func() {
		// Not t.Context: it is already cancelled by the time cleanup runs.
		require.NoError(t, x.Shutdown(context.Background()))
	})

	return &harness{otx: x, logs: logs, spans: spans}
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

// requireSpanIDs asserts that the record carries the ids of span, both in the
// dedicated record fields the SDK fills in and in the attributes the log
// package adds for plain slog handlers.
func requireSpanIDs(t *testing.T, r sdklog.Record, span trace.SpanContext) {
	t.Helper()

	require.Equal(t, span.TraceID(), r.TraceID())
	require.Equal(t, span.SpanID(), r.SpanID())
	require.Equal(t, span.TraceID().String(), attr(t, r, "trace_id").AsString())
	require.Equal(t, span.SpanID().String(), attr(t, r, "span_id").AsString())
}

// requireNoSpanIDs asserts that the record was written with no span active.
func requireNoSpanIDs(t *testing.T, r sdklog.Record) {
	t.Helper()

	require.False(t, r.TraceID().IsValid(), "want no trace id on %q", r.Body().AsString())
	require.False(t, r.SpanID().IsValid(), "want no span id on %q", r.Body().AsString())
	requireNoAttr(t, r, "trace_id")
	requireNoAttr(t, r, "span_id")
}

// apiRecorder is a [go.opentelemetry.io/otel/log.LoggerProvider] that keeps
// the records exactly as the log API hands them over.
//
// The SDK collapses attributes that share a key, keeping the last, so a record
// written with the same attribute twice is indistinguishable from a correct one
// at the exporter. This provider sits above that and is the only place a
// duplicated attribute is still visible.
type apiRecorder struct {
	embedded.LoggerProvider

	mu      sync.Mutex
	records []otellog.Record
}

var _ otellog.LoggerProvider = (*apiRecorder)(nil)

func (p *apiRecorder) Logger(name string, opts ...otellog.LoggerOption) otellog.Logger {
	return apiLogger{recorder: p}
}

type apiLogger struct {
	embedded.Logger

	recorder *apiRecorder
}

var _ otellog.Logger = apiLogger{}

func (l apiLogger) Enabled(ctx context.Context, param otellog.EnabledParameters) bool {
	return true
}

func (l apiLogger) Emit(ctx context.Context, record otellog.Record) {
	l.recorder.mu.Lock()
	defer l.recorder.mu.Unlock()
	l.recorder.records = append(l.recorder.records, record.Clone())
}

// record returns the one record whose body is msg.
func (p *apiRecorder) record(t *testing.T, msg string) otellog.Record {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		found otellog.Record
		n     int
	)
	for _, r := range p.records {
		if r.Body().AsString() != msg {
			continue
		}

		found = r
		n++
	}

	require.Equalf(t, 1, n, "want exactly one %q record, got %d", msg, n)
	return found
}

// apiAttrCount reports how many attributes of r carry the given key.
func apiAttrCount(r otellog.Record, key string) int {
	n := 0
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Key == key {
			n++
		}

		return true
	})

	return n
}

// roundTripperFunc is a stub [net/http.RoundTripper]. Most tests in this
// package use one; the ones in e2e_test.go go over a real socket.
type roundTripperFunc func(r *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// spin burns at least d of the monotonic clock. It is how a test that wants a
// duration to be recorded spends one, without a sleep and without depending on
// the granularity of the clock: whatever the implementation measures around
// the call must be at least d.
func spin(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
	}
}

// meterRecorder is a [go.opentelemetry.io/otel/metric.MeterProvider] that
// remembers which scopes asked it for a meter, which instruments they created
// and every value recorded into them.
//
// The SDK meter provider lives in a module this one does not require, so the
// meter wiring is asserted at the API instead. Everything not overridden below
// delegates to the API noop, which is what makes the type usable as a real
// provider.
type meterRecorder struct {
	metricembedded.MeterProvider

	mu          sync.Mutex
	scopes      []string
	instruments []string
	values      map[string][]float64
}

var _ metric.MeterProvider = (*meterRecorder)(nil)

func (p *meterRecorder) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	p.mu.Lock()
	p.scopes = append(p.scopes, name)
	p.mu.Unlock()

	return recordingMeter{
		Meter:    metricnoop.NewMeterProvider().Meter(name, opts...),
		recorder: p,
	}
}

func (p *meterRecorder) noteInstrument(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instruments = append(p.instruments, name)
}

func (p *meterRecorder) noteValue(name string, v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.values == nil {
		p.values = map[string][]float64{}
	}

	p.values[name] = append(p.values[name], v)
}

// scopeNames returns the instrumentation scopes that asked for a meter.
func (p *meterRecorder) scopeNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.scopes...)
}

// instrumentNames returns the instruments created on this provider.
func (p *meterRecorder) instrumentNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]string(nil), p.instruments...)
}

// valuesOf returns every value recorded into the named instrument.
func (p *meterRecorder) valuesOf(name string) []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]float64(nil), p.values[name]...)
}

type recordingMeter struct {
	metric.Meter

	recorder *meterRecorder
}

func (m recordingMeter) Int64Histogram(name string, opts ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	m.recorder.noteInstrument(name)

	v, err := m.Meter.Int64Histogram(name, opts...)
	return recordingInt64Histogram{Int64Histogram: v, name: name, recorder: m.recorder}, err
}

func (m recordingMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	m.recorder.noteInstrument(name)

	v, err := m.Meter.Float64Histogram(name, opts...)
	return recordingFloat64Histogram{Float64Histogram: v, name: name, recorder: m.recorder}, err
}

type recordingInt64Histogram struct {
	metric.Int64Histogram

	name     string
	recorder *meterRecorder
}

func (h recordingInt64Histogram) Record(ctx context.Context, v int64, opts ...metric.RecordOption) {
	h.recorder.noteValue(h.name, float64(v))
	h.Int64Histogram.Record(ctx, v, opts...)
}

type recordingFloat64Histogram struct {
	metric.Float64Histogram

	name     string
	recorder *meterRecorder
}

func (h recordingFloat64Histogram) Record(ctx context.Context, v float64, opts ...metric.RecordOption) {
	h.recorder.noteValue(h.name, v)
	h.Float64Histogram.Record(ctx, v, opts...)
}
