package otx_test

import (
	"context"

	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// The stubs below record what otx asks of them. Each provider hands out a
// freshly allocated instrument so that "the instrument came from this
// provider" can be asserted by pointer identity, and counts its lifecycle
// calls so that "exactly once" can be asserted.

// lifecycleTrace appends "<event>:<name>" to a slice shared by every stub of a
// test, so that the *relative order* of the lifecycle calls otx makes across
// the controller and the three providers can be asserted, not just their
// counts.
type lifecycleTrace struct {
	name  string
	trace *[]string
}

func (r lifecycleTrace) record(event string) {
	if r.trace == nil {
		return
	}

	*r.trace = append(*r.trace, event+":"+r.name)
}

type stubTracer struct {
	tracenoop.Tracer
}

type stubTracerProvider struct {
	tracenoop.TracerProvider
	lifecycleTrace

	scope_name string
	scope_cfg  trace.TracerConfig
	tracer     *stubTracer

	shutdown_n   int
	shutdown_err error
	flush_n      int
	flush_err    error
}

func (p *stubTracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	p.scope_name = name
	p.scope_cfg = trace.NewTracerConfig(opts...)
	p.tracer = &stubTracer{}

	return p.tracer
}

func (p *stubTracerProvider) Shutdown(ctx context.Context) error {
	p.shutdown_n++
	p.record("shutdown")

	return p.shutdown_err
}

func (p *stubTracerProvider) ForceFlush(ctx context.Context) error {
	p.flush_n++
	p.record("flush")

	return p.flush_err
}

type stubMeter struct {
	metricnoop.Meter
}

type stubMeterProvider struct {
	metricnoop.MeterProvider
	lifecycleTrace

	scope_name string
	scope_cfg  metric.MeterConfig
	meter      *stubMeter

	shutdown_n   int
	shutdown_err error
	flush_n      int
	flush_err    error
}

func (p *stubMeterProvider) Meter(name string, opts ...metric.MeterOption) metric.Meter {
	p.scope_name = name
	p.scope_cfg = metric.NewMeterConfig(opts...)
	p.meter = &stubMeter{}

	return p.meter
}

func (p *stubMeterProvider) Shutdown(ctx context.Context) error {
	p.shutdown_n++
	p.record("shutdown")

	return p.shutdown_err
}

func (p *stubMeterProvider) ForceFlush(ctx context.Context) error {
	p.flush_n++
	p.record("flush")

	return p.flush_err
}

type stubLogger struct {
	lognoop.Logger
}

type stubLoggerProvider struct {
	lognoop.LoggerProvider
	lifecycleTrace

	scope_name string
	scope_cfg  log.LoggerConfig
	logger     *stubLogger

	shutdown_n   int
	shutdown_err error
	flush_n      int
	flush_err    error
}

func (p *stubLoggerProvider) Logger(name string, opts ...log.LoggerOption) log.Logger {
	// otelslog.NewHandler asks for a logger of its own, so only the first
	// question - the one Otx asks - is recorded.
	if p.logger == nil {
		p.scope_name = name
		p.scope_cfg = log.NewLoggerConfig(opts...)
		p.logger = &stubLogger{}
	}

	return p.logger
}

func (p *stubLoggerProvider) Shutdown(ctx context.Context) error {
	p.shutdown_n++
	p.record("shutdown")

	return p.shutdown_err
}

func (p *stubLoggerProvider) ForceFlush(ctx context.Context) error {
	p.flush_n++
	p.record("flush")

	return p.flush_err
}

// plainTracerProvider has neither Shutdown nor ForceFlush, so owning it must
// register no lifecycle hook at all.
type plainTracerProvider struct {
	tracenoop.TracerProvider
}

// dualProvider is a single object serving two signals at once, the shape a
// vendor SDK that exposes one handle for everything would have.
type dualProvider struct {
	tracenoop.TracerProvider
	lognoop.LoggerProvider

	shutdown_n int
	flush_n    int
}

func (p *dualProvider) Shutdown(ctx context.Context) error {
	p.shutdown_n++
	return nil
}

func (p *dualProvider) ForceFlush(ctx context.Context) error {
	p.flush_n++
	return nil
}

type stubPropagator struct {
	fields   []string
	inject_n int
}

func (p *stubPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	p.inject_n++
	carrier.Set("stub", "1")
}

func (p *stubPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return ctx
}

func (p *stubPropagator) Fields() []string {
	return p.fields
}

// closablePropagator is a propagator that happens to have the same lifecycle
// methods a provider has. WithPropagator takes no ownership, so neither must
// ever be called: "has a Shutdown method" is not what makes something owned.
type closablePropagator struct {
	stubPropagator

	shutdown_n int
	flush_n    int
}

func (p *closablePropagator) Shutdown(ctx context.Context) error {
	p.shutdown_n++
	return nil
}

func (p *closablePropagator) ForceFlush(ctx context.Context) error {
	p.flush_n++
	return nil
}

// stubController records Start and Shutdown. It deliberately has no ForceFlush
// method: Otx.ForceFlush reaches the controller through an optional interface
// and both sides of that type assertion need covering.
type stubController struct {
	name  string
	trace *[]string

	start_n    int
	shutdown_n int

	start_err    error
	shutdown_err error
}

func (c *stubController) Start(ctx context.Context) error {
	c.start_n++
	c.record("start:" + c.name)

	return c.start_err
}

func (c *stubController) Shutdown(ctx context.Context) error {
	c.shutdown_n++
	c.record("shutdown:" + c.name)

	return c.shutdown_err
}

func (c *stubController) record(v string) {
	if c.trace == nil {
		return
	}

	*c.trace = append(*c.trace, v)
}

// flushController is a stubController that also implements ForceFlush.
type flushController struct {
	stubController

	flush_n   int
	flush_err error
}

func (c *flushController) ForceFlush(ctx context.Context) error {
	c.flush_n++
	c.record("flush:" + c.name)

	return c.flush_err
}

func logRecord(body string) log.Record {
	var r log.Record
	r.SetSeverity(log.SeverityInfo)
	r.SetBody(log.StringValue(body))

	return r
}

// uncomparableProvider is a value type that cannot be compared with ==, which
// is what makes the identity check in isSameAny fall back to registering its
// hooks. The counter is shared through a pointer because the value itself is
// copied into the interface.
type uncomparableProvider struct {
	tracenoop.TracerProvider
	lognoop.LoggerProvider

	// A slice field is what makes the struct uncomparable.
	_ []int

	shutdown_n *int
}

func (p uncomparableProvider) Shutdown(ctx context.Context) error {
	*p.shutdown_n++
	return nil
}
