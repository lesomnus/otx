package otx_test

import (
	"context"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestWithProvider(t *testing.T) {
	t.Run("the tracer provider sets both the provider and the tracer", func(t *testing.T) {
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithTracerProvider(tp))

		require.Same(t, tp, x.Providers().Tracer())
		require.Same(t, tp.tracer, x.Tracer())
	})
	t.Run("the meter provider sets both the provider and the meter", func(t *testing.T) {
		mp := &stubMeterProvider{}
		x := otx.New(otx.WithMeterProvider(mp))

		require.Same(t, mp, x.Providers().Meter())
		require.Same(t, mp.meter, x.Meter())
	})
	t.Run("the logger provider sets both the provider and the logger", func(t *testing.T) {
		lp := &stubLoggerProvider{}
		x := otx.New(otx.WithLoggerProvider(lp))

		require.Same(t, lp, x.Providers().Logger())
		require.Same(t, lp.logger, x.Logger())
	})
	t.Run("one option leaves the other two on their defaults", func(t *testing.T) {
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithTracerProvider(tp))

		require.Same(t, otel.GetMeterProvider(), x.Providers().Meter())
		require.Same(t, logglobal.GetLoggerProvider(), x.Providers().Logger())
	})
	t.Run("the propagator is replaced wholesale", func(t *testing.T) {
		pr := &stubPropagator{fields: []string{"x-stub"}}
		x := otx.New(otx.WithPropagator(pr))

		require.Same(t, pr, x.Propagator())
		require.Equal(t, []string{"x-stub"}, x.Propagator().Fields())
	})
}

// TestWithNil is a regression test. Handing an option a nil - most often the
// typed nil a "return nil, err" constructor produces on a path whose error was
// ignored - used to be stored, turning a wiring mistake into a nil dereference
// at the first use of the instrument.
func TestWithNil(t *testing.T) {
	tracer_provider := otel.GetTracerProvider()
	meter_provider := otel.GetMeterProvider()
	logger_provider := logglobal.GetLoggerProvider()

	requireDefaults := func(t *testing.T, x *otx.Otx) {
		t.Helper()

		require.NotNil(t, x)
		require.Same(t, tracer_provider, x.Providers().Tracer())
		require.Same(t, meter_provider, x.Providers().Meter())
		require.Same(t, logger_provider, x.Providers().Logger())

		require.NotNil(t, x.Tracer())
		require.NotNil(t, x.Meter())
		require.NotNil(t, x.Logger())
		require.NotNil(t, x.SlogHandler())
		require.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, x.Propagator().Fields())

		// The instruments are real enough to use.
		_, span := x.TraceStart(t.Context(), "op")
		span.End()
		x.Logger().Emit(t.Context(), logRecord("op"))

		require.NoError(t, x.Start(t.Context()))
		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(t.Context()))
	}

	t.Run("a nil interface leaves the default in place", func(t *testing.T) {
		var x *otx.Otx
		require.NotPanics(t, func() {
			x = otx.New(
				otx.WithTracerProvider(nil),
				otx.WithMeterProvider(nil),
				otx.WithLoggerProvider(nil),
				otx.WithPropagator(nil),
				otx.WithController(nil),
			)
		})

		requireDefaults(t, x)
	})
	t.Run("a typed nil leaves the default in place", func(t *testing.T) {
		var (
			tp *sdktrace.TracerProvider
			mp *stubMeterProvider
			lp *sdklog.LoggerProvider
			pr *stubPropagator
			c  *stubController
		)

		var x *otx.Otx
		require.NotPanics(t, func() {
			x = otx.New(
				otx.WithTracerProvider(tp),
				otx.WithMeterProvider(mp),
				otx.WithLoggerProvider(lp),
				otx.WithPropagator(pr),
				otx.WithController(c),
			)
		})

		requireDefaults(t, x)
	})
	t.Run("a value type is never mistaken for nil", func(t *testing.T) {
		// The nil screen reflects on the value it is given, so it has to leave
		// the kinds that cannot be nil alone. propagation.TraceContext is a
		// struct, and dropping it would silently restore the default composite.
		x := otx.New(otx.WithPropagator(propagation.TraceContext{}))

		require.Equal(t, []string{"traceparent", "tracestate"}, x.Propagator().Fields())
		require.NotContains(t, x.Propagator().Fields(), "baggage", "the default composite was not kept")

		// A controller is screened by the same helper, and NewController
		// returns a struct value rather than a pointer.
		called := false
		y := otx.New(otx.WithController(otx.NewController(
			func(ctx context.Context) error { called = true; return nil },
			nil,
		)))
		require.NoError(t, y.Start(t.Context()))
		require.True(t, called)
	})
	t.Run("a nil Option in the list is skipped", func(t *testing.T) {
		var x *otx.Otx
		require.NotPanics(t, func() {
			x = otx.New(nil, otx.WithScopeName("app"), nil, otx.WithScopeVersion("v1"), nil)
		})

		require.NotNil(t, x)

		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
		x = otx.New(nil, otx.WithTracerProvider(tp), nil, otx.WithScopeName("app"), nil)

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.Len(t, sr.Ended(), 1)
		require.Equal(t, "app", sr.Ended()[0].InstrumentationScope().Name)
	})
}

func TestWithScope(t *testing.T) {
	newOtx := func(t *testing.T, opts ...otx.Option) (*tracetest.SpanRecorder, *otxmem.LogExporter, *otx.Otx) {
		t.Helper()

		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

		exporter := &otxmem.LogExporter{}
		lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))

		opts = append([]otx.Option{
			otx.WithTracerProvider(tp),
			otx.WithLoggerProvider(lp),
		}, opts...)

		return sr, exporter, otx.New(opts...)
	}

	t.Run("the scope of the emitted telemetry follows the options", func(t *testing.T) {
		sr, exporter, x := newOtx(t,
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v4.5.6"),
			otx.WithScopeSchemaURL("https://example.test/schema"),
		)

		_, span := x.TraceStart(t.Context(), "op")
		span.End()
		x.Logger().Emit(t.Context(), logRecord("op"))

		require.Len(t, sr.Ended(), 1)
		scope := sr.Ended()[0].InstrumentationScope()
		require.Equal(t, "app", scope.Name)
		require.Equal(t, "v4.5.6", scope.Version)
		require.Equal(t, "https://example.test/schema", scope.SchemaURL)

		require.Equal(t, 1, exporter.Len())
		log_scope := exporter.Records()[0].InstrumentationScope()
		require.Equal(t, "app", log_scope.Name)
		require.Equal(t, "v4.5.6", log_scope.Version)
		require.Equal(t, "https://example.test/schema", log_scope.SchemaURL)
	})
	t.Run("an empty name is ignored", func(t *testing.T) {
		sr, _, x := newOtx(t, otx.WithScopeName(""))

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.Equal(t, otx.Scope, sr.Ended()[0].InstrumentationScope().Name)
	})
	t.Run("an empty version clears it", func(t *testing.T) {
		// Unlike the name, an empty version is taken at face value: it is the
		// documented way to emit telemetry with no scope version at all.
		sr, _, x := newOtx(t, otx.WithScopeVersion("v1"), otx.WithScopeVersion(""))

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.Empty(t, sr.Ended()[0].InstrumentationScope().Version)
	})
	t.Run("an empty schema url clears it", func(t *testing.T) {
		sr, _, x := newOtx(t, otx.WithScopeSchemaURL("https://example.test/schema"), otx.WithScopeSchemaURL(""))

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.Empty(t, sr.Ended()[0].InstrumentationScope().SchemaURL)
	})
	t.Run("the scope applies whatever order the options come in", func(t *testing.T) {
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithScopeName("app"), otx.WithTracerProvider(tp))

		require.Equal(t, "app", tp.scope_name)
		require.Same(t, tp.tracer, x.Tracer())
	})
}

// TestOptionRepeated pins the fix for a provider being registered for shutdown
// once per call rather than once per signal.
func TestOptionRepeated(t *testing.T) {
	ctx := t.Context()

	t.Run("the last tracer provider wins and is owned exactly once", func(t *testing.T) {
		first := &stubTracerProvider{}
		last := &stubTracerProvider{}

		x := otx.New(otx.WithTracerProvider(first), otx.WithTracerProvider(last))
		require.Same(t, last, x.Providers().Tracer())
		require.Same(t, last.tracer, x.Tracer())

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, last.shutdown_n)
		require.Equal(t, 1, last.flush_n)

		// The discarded provider is not this Otx's to close.
		require.Zero(t, first.shutdown_n)
		require.Zero(t, first.flush_n)
	})
	t.Run("the last meter provider wins and is owned exactly once", func(t *testing.T) {
		first := &stubMeterProvider{}
		last := &stubMeterProvider{}

		x := otx.New(otx.WithMeterProvider(first), otx.WithMeterProvider(last))
		require.Same(t, last, x.Providers().Meter())

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, last.shutdown_n)
		require.Equal(t, 1, last.flush_n)
		require.Zero(t, first.shutdown_n)
		require.Zero(t, first.flush_n)
	})
	t.Run("the last logger provider wins and is owned exactly once", func(t *testing.T) {
		first := &stubLoggerProvider{}
		last := &stubLoggerProvider{}

		x := otx.New(otx.WithLoggerProvider(first), otx.WithLoggerProvider(last))
		require.Same(t, last, x.Providers().Logger())

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, last.shutdown_n)
		require.Equal(t, 1, last.flush_n)
		require.Zero(t, first.shutdown_n)
		require.Zero(t, first.flush_n)
	})
	t.Run("the last controller wins", func(t *testing.T) {
		first := &stubController{name: "first"}
		last := &stubController{name: "last"}

		x := otx.New(otx.WithController(first), otx.WithController(last))
		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, last.start_n)
		require.Equal(t, 1, last.shutdown_n)
		require.Zero(t, first.start_n)
		require.Zero(t, first.shutdown_n)
	})
	t.Run("the last propagator wins", func(t *testing.T) {
		first := &stubPropagator{fields: []string{"first"}}
		last := &stubPropagator{fields: []string{"last"}}

		x := otx.New(otx.WithPropagator(first), otx.WithPropagator(last))
		require.Same(t, last, x.Propagator())
	})
	t.Run("a nil does not undo the provider already given", func(t *testing.T) {
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithTracerProvider(tp), otx.WithTracerProvider(nil))

		require.Same(t, tp, x.Providers().Tracer())
		require.NoError(t, x.Shutdown(ctx))
		require.Equal(t, 1, tp.shutdown_n)
	})
}
