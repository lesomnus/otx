// Package otxsdk builds a configured [otx.Otx] from the standard OTEL_*
// environment variables, so that an application does not have to wire the
// OpenTelemetry SDK by hand.
//
//	func main() {
//		ctx := context.Background()
//
//		x, err := otxsdk.New(ctx)
//		if err != nil {
//			log.Fatal(err)
//		}
//		defer x.Shutdown(ctx)
//
//		ctx = otx.Into(ctx, x)
//		// ...
//	}
//
// It is a separate module because it pulls in the SDK and every exporter;
// programs that are handed providers by their host do not need any of it.
//
// # Configuration
//
// The variables read here select what to export and how to talk to the
// collector:
//
//	OTEL_SDK_DISABLED                    true turns all three signals into no-ops
//	OTEL_TRACES_EXPORTER                 otlp (default), console, none
//	OTEL_METRICS_EXPORTER                otlp (default), console, none
//	OTEL_LOGS_EXPORTER                   otlp (default), console, none
//	OTEL_EXPORTER_OTLP_PROTOCOL          http/protobuf (default), grpc
//	OTEL_EXPORTER_OTLP_TRACES_PROTOCOL   overrides the above for traces
//	OTEL_EXPORTER_OTLP_METRICS_PROTOCOL  overrides the above for metrics
//	OTEL_EXPORTER_OTLP_LOGS_PROTOCOL     overrides the above for logs
//
// An exporter variable may list several, comma separated, so that
// OTEL_TRACES_EXPORTER=otlp,console sends spans to the collector and prints
// them.
//
// Everything else is read by the SDK itself and is documented by it:
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES for the resource,
// OTEL_EXPORTER_OTLP_ENDPOINT, _HEADERS, _TIMEOUT and _COMPRESSION for the
// connection, OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG for sampling,
// and the OTEL_BSP_*, OTEL_BLRP_* and OTEL_METRIC_EXPORT_* families for
// batching.
package otxsdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/otel/attribute"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// New builds providers for all three signals from the environment and returns
// an [otx.Otx] that owns them: shutting it down flushes and stops everything
// this call created.
//
// If a provider cannot be built, the ones already built are shut down and the
// error is returned, so a failed call leaves nothing running.
func New(ctx context.Context, opts ...Option) (*otx.Otx, error) {
	c := config{}
	for _, f := range opts {
		if f == nil {
			continue
		}
		f(&c)
	}

	if disabled() {
		return otx.New(append([]otx.Option{
			otx.WithTracerProvider(tracenoop.NewTracerProvider()),
			otx.WithMeterProvider(metricnoop.NewMeterProvider()),
			otx.WithLoggerProvider(lognoop.NewLoggerProvider()),
		}, c.otx...)...), nil
	}

	res, err := c.newResource(ctx)
	if err != nil {
		return nil, fmt.Errorf("otxsdk: resource: %w", err)
	}

	// Providers are collected as they are built so that a later failure can
	// unwind the earlier ones.
	built := []any{}
	fail := func(err error) (*otx.Otx, error) {
		errs := []error{err}
		for i := len(built) - 1; i >= 0; i-- {
			f := shutdownOf(built[i])
			if f == nil {
				continue
			}
			// Reported, but marked as secondary: shutting down a provider that
			// has just been built usually means a final export attempt, so
			// with no collector reachable this is noisy and is never the cause.
			if err := f(ctx); err != nil {
				errs = append(errs, fmt.Errorf("otxsdk: while unwinding: %w", err))
			}
		}

		return nil, errors.Join(errs...)
	}

	tracer_provider, err := newTracerProvider(ctx, res)
	if err != nil {
		return fail(fmt.Errorf("otxsdk: traces: %w", err))
	}
	built = append(built, tracer_provider)

	meter_provider, err := newMeterProvider(ctx, res)
	if err != nil {
		return fail(fmt.Errorf("otxsdk: metrics: %w", err))
	}
	built = append(built, meter_provider)

	logger_provider, err := newLoggerProvider(ctx, res)
	if err != nil {
		return fail(fmt.Errorf("otxsdk: logs: %w", err))
	}
	built = append(built, logger_provider)

	return otx.New(append([]otx.Option{
		otx.WithTracerProvider(tracer_provider),
		otx.WithMeterProvider(meter_provider),
		otx.WithLoggerProvider(logger_provider),
	}, c.otx...)...), nil
}

// Option configures [New].
type Option func(*config)

type config struct {
	resource       *resource.Resource
	resource_attrs []attribute.KeyValue
	otx            []otx.Option
}

// WithResource sets the resource this package builds, instead of deriving one
// from the environment. A nil resource is ignored.
//
// It does not stop OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES from
// reaching the providers: each SDK merges [resource.Environment] underneath
// whatever resource it is handed, and this package cannot turn that off.
func WithResource(res *resource.Resource) Option {
	return func(c *config) {
		if res == nil {
			return
		}

		c.resource = res
	}
}

// WithResourceAttributes adds attributes to the resource built from the
// environment. It is ignored when [WithResource] is given.
func WithResourceAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) {
		c.resource_attrs = append(c.resource_attrs, attrs...)
	}
}

// WithOtxOptions passes options through to [otx.New], after the providers this
// package builds. Use it for the settings that are not part of the SDK, such
// as [otx.WithScopeName] and [otx.WithPropagator].
func WithOtxOptions(opts ...otx.Option) Option {
	return func(c *config) {
		c.otx = append(c.otx, opts...)
	}
}

func (c config) newResource(ctx context.Context) (*resource.Resource, error) {
	if c.resource != nil {
		return c.resource, nil
	}

	// The detectors are merged in the order they are given, later winning, so
	// the fallback service name is first and anything real replaces it.
	//
	// resource.Default supplies the same fallback but is computed once per
	// process, which makes the result depend on the environment of whichever
	// call came first. Detecting it here keeps New a function of the
	// environment as it is now.
	return resource.New(ctx,
		resource.WithDetectors(defaultServiceName()),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithAttributes(c.resource_attrs...),
	)
}

// defaultServiceName detects the service.name the specification asks for when
// nothing else supplies one: unknown_service:<binary>. It carries no schema
// URL, so it merges with a resource of any schema.
func defaultServiceName() resource.Detector {
	return resource.StringDetector("", semconv.ServiceNameKey, func() (string, error) {
		executable, err := os.Executable()
		if err != nil {
			return "unknown_service:go", nil
		}

		return "unknown_service:" + filepath.Base(executable), nil
	})
}

func shutdownOf(v any) func(context.Context) error {
	if p, ok := v.(interface {
		Shutdown(context.Context) error
	}); ok {
		return p.Shutdown
	}

	return nil
}
