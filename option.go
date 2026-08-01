package otx

import (
	"reflect"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Option configures an [Otx] during [New].
type Option func(otx *Otx)

// WithController sets the lifecycle hook run by [Otx.Start] and
// [Otx.Shutdown]. A nil controller is ignored.
//
// Use [JoinControllers] to give more than one.
func WithController(controller Controller) Option {
	return func(otx *Otx) {
		if isNil(controller) {
			return
		}

		otx.controller = controller
	}
}

// WithTracerProvider sets the tracer provider and takes ownership of it: its
// Shutdown and ForceFlush methods, if any, are run by [Otx.Shutdown] and
// [Otx.ForceFlush]. A nil provider is ignored, leaving the default in place.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(otx *Otx) {
		if isNil(provider) {
			return
		}

		otx.providers.tracer_provider = provider
		otx.owned[0] = provider
	}
}

// WithMeterProvider sets the meter provider and takes ownership of it, as
// [WithTracerProvider] does for traces. A nil provider is ignored.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(otx *Otx) {
		if isNil(provider) {
			return
		}

		otx.providers.meter_provider = provider
		otx.owned[1] = provider
	}
}

// WithLoggerProvider sets the logger provider and takes ownership of it, as
// [WithTracerProvider] does for traces. A nil provider is ignored.
func WithLoggerProvider(provider log.LoggerProvider) Option {
	return func(otx *Otx) {
		if isNil(provider) {
			return
		}

		otx.providers.logger_provider = provider
		otx.owned[2] = provider
	}
}

// WithPropagator sets the propagator used to carry trace context across
// process boundaries. A nil propagator is ignored.
//
// The default is a composite of [propagation.TraceContext] and
// [propagation.Baggage]. Pass [go.opentelemetry.io/otel.GetTextMapPropagator]()
// to defer to the OpenTelemetry global instead.
func WithPropagator(propagator propagation.TextMapPropagator) Option {
	return func(otx *Otx) {
		if isNil(propagator) {
			return
		}

		otx.propagator = propagator
	}
}

// WithScopeName sets the instrumentation scope name of every instrument, which
// defaults to [Scope]. An empty name is ignored.
func WithScopeName(name string) Option {
	return func(otx *Otx) {
		if name == "" {
			return
		}

		otx.scope.name = name
	}
}

// WithScopeVersion sets the instrumentation scope version, which defaults to
// the version of this module as recorded in the build info.
func WithScopeVersion(version string) Option {
	return func(otx *Otx) {
		otx.scope.version = version
	}
}

// WithScopeSchemaURL sets the instrumentation scope schema URL, which is empty
// by default.
func WithScopeSchemaURL(url string) Option {
	return func(otx *Otx) {
		otx.scope.schema_url = url
	}
}

// isSameAny reports whether vs already holds v. A provider type that cannot be
// compared is treated as not present, which at worst registers its hooks twice
// - the same as before this check existed, and harmless because Shutdown is
// idempotent for every provider the SDK ships.
func isSameAny(vs []any, v any) bool {
	if v == nil {
		return false
	}

	t := reflect.TypeOf(v)
	if !t.Comparable() {
		return false
	}
	for _, o := range vs {
		if o != nil && reflect.TypeOf(o) == t && o == v {
			return true
		}
	}

	return false
}

// isNil reports whether v is a nil interface or an interface holding a nil
// pointer. The typed-nil case is the common one: it is what a constructor that
// returned "nil, err" on an ignored error path hands to an option, and
// assigning it would turn a wiring mistake into a nil dereference at the first
// use of the instrument.
func isNil(v any) bool {
	if v == nil {
		return true
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.UnsafePointer, reflect.Interface, reflect.Slice:
		return rv.IsNil()

	default:
		return false
	}
}
