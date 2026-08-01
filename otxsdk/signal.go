package otxsdk

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func newTracerProvider(ctx context.Context, res *resource.Resource) (trace.TracerProvider, error) {
	names, err := exporterNames("OTEL_TRACES_EXPORTER")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return tracenoop.NewTracerProvider(), nil
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	for _, name := range names {
		exporter, err := newSpanExporter(ctx, name)
		if err != nil {
			return nil, err
		}

		opts = append(opts, sdktrace.WithBatcher(exporter))
	}

	return sdktrace.NewTracerProvider(opts...), nil
}

func newSpanExporter(ctx context.Context, name string) (sdktrace.SpanExporter, error) {
	if name == exporterConsole {
		return stdouttrace.New()
	}

	protocol, err := protocolName("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if err != nil {
		return nil, err
	}
	if protocol == protocolGRPC {
		return otlptracegrpc.New(ctx)
	}

	return otlptracehttp.New(ctx)
}

func newMeterProvider(ctx context.Context, res *resource.Resource) (metric.MeterProvider, error) {
	names, err := exporterNames("OTEL_METRICS_EXPORTER")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return metricnoop.NewMeterProvider(), nil
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, name := range names {
		exporter, err := newMetricExporter(ctx, name)
		if err != nil {
			return nil, err
		}

		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}

	return sdkmetric.NewMeterProvider(opts...), nil
}

func newMetricExporter(ctx context.Context, name string) (sdkmetric.Exporter, error) {
	if name == exporterConsole {
		return stdoutmetric.New()
	}

	protocol, err := protocolName("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if err != nil {
		return nil, err
	}
	if protocol == protocolGRPC {
		return otlpmetricgrpc.New(ctx)
	}

	return otlpmetrichttp.New(ctx)
}

func newLoggerProvider(ctx context.Context, res *resource.Resource) (otellog.LoggerProvider, error) {
	names, err := exporterNames("OTEL_LOGS_EXPORTER")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return lognoop.NewLoggerProvider(), nil
	}

	opts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	for _, name := range names {
		exporter, err := newLogExporter(ctx, name)
		if err != nil {
			return nil, err
		}

		opts = append(opts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	}

	return sdklog.NewLoggerProvider(opts...), nil
}

func newLogExporter(ctx context.Context, name string) (sdklog.Exporter, error) {
	if name == exporterConsole {
		return stdoutlog.New()
	}

	protocol, err := protocolName("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL")
	if err != nil {
		return nil, err
	}
	if protocol == protocolGRPC {
		return otlploggrpc.New(ctx)
	}

	return otlploghttp.New(ctx)
}
