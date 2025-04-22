package otx

import (
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Option func(otx *Otx)

func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(otx *Otx) {
		otx.tracer_provider = provider
		otx.tracer = provider.Tracer(Scope)
	}
}

func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(otx *Otx) {
		otx.meter_provider = provider
		otx.meter = provider.Meter(Scope)
	}
}

func WithLoggerProvider(provider log.LoggerProvider) Option {
	return func(otx *Otx) {
		otx.logger_provider = provider
		otx.logger = provider.Logger(Scope)
	}
}
