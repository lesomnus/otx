// Package otx carries the three OpenTelemetry signal providers - traces,
// metrics and logs - plus a propagator as a single value through a
// [context.Context].
//
// An [Otx] is created once at startup with [New], handed to the middleware of
// the transport in use (see the otxgrpc and otxhttp modules), and read back out
// of the request context with [From] or one of the package level accessors such
// as [Tracer], [Meter] and [TraceStart].
//
// Every accessor degrades gracefully: on a context that carries no [Otx] they
// fall back to a shared instance built from the OpenTelemetry globals rather
// than panicking. Only the lifecycle functions ([Start], [Shutdown] and
// [ForceFlush]) report the missing value, as [ErrNoOtx], because silently
// reporting a successful shutdown of nothing is worse than an error.
package otx

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Scope is the default instrumentation scope name given to every instrument
// [New] creates. Override it with [WithScopeName] to attribute telemetry to
// the application instead of to this library.
const Scope string = "github.com/lesomnus/otx"

type ctxKey struct{}

// ErrNoOtx is returned by the package level [Start], [Shutdown] and
// [ForceFlush] when the given context carries no [Otx]. It signals a wiring
// mistake: the lifecycle of an [Otx] is a process concern and the value is
// normally held directly rather than looked up.
var ErrNoOtx = errors.New("otx: no Otx in the context")

// Otx holds a set of OpenTelemetry providers, the instruments derived from
// them, and the propagator used to carry trace context across process
// boundaries.
//
// An Otx is immutable after [New] returns and is safe for concurrent use. The
// zero value is not usable; always construct one with [New].
type Otx struct {
	controller Controller
	providers  providerSet
	propagator propagation.TextMapPropagator
	scope      scope

	tracer       trace.Tracer
	meter        metric.Meter
	logger       log.Logger
	slog_handler slog.Handler

	// Metric instruments created through the constructors in instrument.go,
	// keyed by instrumentKey.
	instruments sync.Map

	// Providers passed to WithTracerProvider, WithMeterProvider and
	// WithLoggerProvider, in that order. Only these are owned by this Otx, and
	// only these contribute lifecycle hooks. Indexed so that repeating an
	// option registers the provider once, not once per call.
	owned [3]any

	// Shutdown and flush hooks collected from owned, in signal order.
	shutdowns []func(context.Context) error
	flushes   []func(context.Context) error

	shutdown_once sync.Once
	shutdown_err  error
}

// New creates an Otx from the given options.
//
// Defaults, all overridable:
//
//   - Tracer, meter and logger providers are the OpenTelemetry globals
//     ([otel.GetTracerProvider], [otel.GetMeterProvider] and
//     [go.opentelemetry.io/otel/log/global.GetLoggerProvider]). All three
//     delegate, so a provider installed globally after this call is still
//     picked up.
//   - The propagator is a composite of [propagation.TraceContext] and
//     [propagation.Baggage]. Note this differs from
//     [otel.GetTextMapPropagator], whose default is an empty composite that
//     silently propagates nothing.
//   - The instrumentation scope is [Scope] at the version of this module.
//
// Providers given through [WithTracerProvider], [WithMeterProvider] and
// [WithLoggerProvider] are owned by the returned Otx: their Shutdown and
// ForceFlush methods, where present, are run by [Otx.Shutdown] and
// [Otx.ForceFlush]. Providers taken from the globals are not, since this Otx
// did not create them.
func New(opts ...Option) *Otx {
	v := &Otx{
		controller: noopController{},

		providers: providerSet{
			tracer_provider: otel.GetTracerProvider(),
			meter_provider:  otel.GetMeterProvider(),
			logger_provider: logglobal.GetLoggerProvider(),
		},

		propagator: propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		),

		scope: scope{name: Scope, version: moduleVersion()},
	}
	for _, f := range opts {
		if f == nil {
			continue
		}
		f(v)
	}

	for i, p := range v.owned {
		// One object can serve more than one signal. Its hooks are registered
		// once, so that a single Shutdown does not shut it down once per
		// signal it was given for.
		if !isSameAny(v.owned[:i], p) {
			v.own(p)
		}
	}

	// Instruments are derived only once every option has run so that each one
	// is guaranteed to come from the provider that is actually stored.
	v.tracer = v.providers.tracer_provider.Tracer(v.scope.name, v.scope.traceOpts()...)
	v.meter = v.providers.meter_provider.Meter(v.scope.name, v.scope.metricOpts()...)
	v.logger = v.providers.logger_provider.Logger(v.scope.name, v.scope.logOpts()...)
	v.slog_handler = otelslog.NewHandler(v.scope.name, v.scope.slogOpts(v.providers.logger_provider)...)

	return v
}

// fallback is the Otx returned by From for a context that carries none. It is
// built once so that a miss costs a single atomic load and always yields the
// same value.
var fallback = sync.OnceValue(func() *Otx { return New() })

// Into returns a copy of ctx carrying v. A nil v leaves ctx unchanged, so that
// a mis-wired call cannot poison the context for every later reader.
func Into(ctx context.Context, v *Otx) context.Context {
	if v == nil {
		return ctx
	}

	return context.WithValue(ctx, ctxKey{}, v)
}

// From returns the Otx carried by ctx, or a shared instance built from the
// OpenTelemetry globals if there is none. It never returns nil.
//
// Use [FromOK] to tell the two cases apart.
func From(ctx context.Context) *Otx {
	if v, ok := FromOK(ctx); ok {
		return v
	}

	return fallback()
}

// FromOK returns the Otx carried by ctx and reports whether there was one.
func FromOK(ctx context.Context) (*Otx, bool) {
	v, ok := ctx.Value(ctxKey{}).(*Otx)
	if !ok || v == nil {
		return nil, false
	}

	return v, true
}

// Start starts the [Controller] of the Otx carried by ctx. It returns
// [ErrNoOtx] if ctx carries none.
func Start(ctx context.Context) error {
	v, ok := FromOK(ctx)
	if !ok {
		return ErrNoOtx
	}

	return v.Start(ctx)
}

// Shutdown shuts down the Otx carried by ctx. It returns [ErrNoOtx] if ctx
// carries none, rather than reporting a successful shutdown of nothing.
func Shutdown(ctx context.Context) error {
	v, ok := FromOK(ctx)
	if !ok {
		return ErrNoOtx
	}

	return v.Shutdown(ctx)
}

// ForceFlush flushes the owned providers of the Otx carried by ctx. It returns
// [ErrNoOtx] if ctx carries none.
func ForceFlush(ctx context.Context) error {
	v, ok := FromOK(ctx)
	if !ok {
		return ErrNoOtx
	}

	return v.ForceFlush(ctx)
}

// Start starts the [Controller] given by [WithController]. It is a no-op
// unless one was given.
func (o *Otx) Start(ctx context.Context) error {
	return o.controller.Start(ctx)
}

// Shutdown shuts down the [Controller] given by [WithController] and then the
// providers this Otx owns, joining every error. Repeated calls return the
// result of the first one.
func (o *Otx) Shutdown(ctx context.Context) error {
	o.shutdown_once.Do(func() {
		errs := make([]error, 0, len(o.shutdowns)+1)
		errs = append(errs, o.controller.Shutdown(ctx))
		for _, f := range o.shutdowns {
			errs = append(errs, f(ctx))
		}

		o.shutdown_err = errors.Join(errs...)
	})

	return o.shutdown_err
}

// ForceFlush flushes the providers this Otx owns, and the [Controller] given
// by [WithController] if it has a ForceFlush method, joining every error.
func (o *Otx) ForceFlush(ctx context.Context) error {
	errs := make([]error, 0, len(o.flushes)+1)
	if v, ok := o.controller.(interface {
		ForceFlush(context.Context) error
	}); ok {
		errs = append(errs, v.ForceFlush(ctx))
	}
	for _, f := range o.flushes {
		errs = append(errs, f(ctx))
	}

	return errors.Join(errs...)
}

// Providers returns the provider set this Otx was built from.
func (o *Otx) Providers() ProviderSet {
	return o.providers
}

// Tracer returns the tracer derived from the tracer provider.
func (o *Otx) Tracer() trace.Tracer {
	return o.tracer
}

// Meter returns the meter derived from the meter provider.
func (o *Otx) Meter() metric.Meter {
	return o.meter
}

// Logger returns the OpenTelemetry logger derived from the logger provider.
// For an [log/slog.Logger], use [github.com/lesomnus/otx/log.From].
func (o *Otx) Logger() log.Logger {
	return o.logger
}

// SlogHandler returns a shared [log/slog.Handler] bridging into the logger
// provider. It is created once by [New].
func (o *Otx) SlogHandler() slog.Handler {
	return o.slog_handler
}

// Propagator returns the propagator used to carry trace context across
// process boundaries.
func (o *Otx) Propagator() propagation.TextMapPropagator {
	return o.propagator
}

// TraceStart starts a span on the tracer of this Otx.
func (o *Otx) TraceStart(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return o.tracer.Start(ctx, name, opts...)
}

// Providers returns the provider set of the Otx carried by ctx.
func Providers(ctx context.Context) ProviderSet {
	return From(ctx).Providers()
}

// Tracer returns the tracer of the Otx carried by ctx.
func Tracer(ctx context.Context) trace.Tracer {
	return From(ctx).Tracer()
}

// TraceStart starts a span on the tracer of the Otx carried by ctx.
func TraceStart(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return From(ctx).TraceStart(ctx, name, opts...)
}

// Meter returns the meter of the Otx carried by ctx.
func Meter(ctx context.Context) metric.Meter {
	return From(ctx).Meter()
}

// Logger returns the OpenTelemetry logger of the Otx carried by ctx.
func Logger(ctx context.Context) log.Logger {
	return From(ctx).Logger()
}

// Propagator returns the propagator of the Otx carried by ctx.
func Propagator(ctx context.Context) propagation.TextMapPropagator {
	return From(ctx).Propagator()
}

// ProviderSet is the set of OpenTelemetry providers held by an [Otx].
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

func (s providerSet) Tracer() trace.TracerProvider {
	return s.tracer_provider
}

func (s providerSet) Meter() metric.MeterProvider {
	return s.meter_provider
}

func (s providerSet) Logger() log.LoggerProvider {
	return s.logger_provider
}

// own registers the lifecycle hooks of a provider passed through an option.
func (o *Otx) own(v any) {
	if v == nil {
		return
	}
	if p, ok := v.(interface {
		Shutdown(context.Context) error
	}); ok {
		o.shutdowns = append(o.shutdowns, p.Shutdown)
	}
	if p, ok := v.(interface {
		ForceFlush(context.Context) error
	}); ok {
		o.flushes = append(o.flushes, p.ForceFlush)
	}
}
