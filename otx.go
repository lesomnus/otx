package otx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const Scope string = "github.com/lesomnus/otx"

type ctxKey struct{}

type Otx struct {
	tracer_provider trace.TracerProvider
	meter_provider  metric.MeterProvider
	logger_provider log.LoggerProvider

	tracer trace.Tracer
	meter  metric.Meter
	logger log.Logger
}

func New(opts ...Option) *Otx {
	v := &Otx{
		tracer_provider: otel.GetTracerProvider(),
		meter_provider:  otel.GetMeterProvider(),
		logger_provider: noop.NewLoggerProvider(),

		tracer: otel.Tracer(Scope),
		meter:  otel.Meter(Scope),
		logger: noop.NewLoggerProvider().Logger(Scope),
	}
	for _, f := range opts {
		f(v)
	}

	return v
}

func (o *Otx) Providers() ProviderSet {
	return providerSet{
		tracer_provider: o.tracer_provider,
		meter_provider:  o.meter_provider,
		logger_provider: o.logger_provider,
	}
}

func Into(ctx context.Context, v *Otx) context.Context {
	return context.WithValue(ctx, ctxKey{}, v)
}

func from(ctx context.Context) *Otx {
	v, ok := ctx.Value(ctxKey{}).(*Otx)
	if !ok {
		return New()
	}

	return v
}

func Tracer(ctx context.Context) trace.Tracer {
	return from(ctx).tracer
}

func TraceStart(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return from(ctx).tracer.Start(ctx, name, opts...)
}

func Meter(ctx context.Context) metric.Meter {
	return from(ctx).meter
}

func Logger(ctx context.Context) log.Logger {
	return from(ctx).logger
}

type ProviderSet interface {
	Tracer() trace.TracerProvider
	Meter() metric.MeterProvider
	Logger() log.LoggerProvider
}

type providerSet struct {
	tracer_provider trace.TracerProvider
	meter_provider  metric.MeterProvider
	logger_provider log.LoggerProvider
}

func Providers(ctx context.Context) ProviderSet {
	return from(ctx).Providers()
}

func (s providerSet) Tracer() trace.TracerProvider {
	return s.tracer_provider
}

func (s providerSet) Meter() metric.MeterProvider {
	return s.meter_provider
}

func (s providerSet) Logger() log.LoggerProvider {
	return s.logger_provider
}
