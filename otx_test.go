package otx_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func sampledSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

func TestNew(t *testing.T) {
	t.Run("providers default to the otel globals", func(t *testing.T) {
		x := otx.New()
		require.NotNil(t, x)

		ps := x.Providers()
		require.Same(t, otel.GetTracerProvider(), ps.Tracer())
		require.Same(t, otel.GetMeterProvider(), ps.Meter())
		require.Same(t, logglobal.GetLoggerProvider(), ps.Logger())
	})
	t.Run("the default scope is this module", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

		x := otx.New(otx.WithTracerProvider(tp))
		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.Len(t, sr.Ended(), 1)
		require.Equal(t, otx.Scope, sr.Ended()[0].InstrumentationScope().Name)
		require.Empty(t, sr.Ended()[0].InstrumentationScope().SchemaURL)
	})
	t.Run("every instrument comes from the stored provider", func(t *testing.T) {
		tp := &stubTracerProvider{}
		mp := &stubMeterProvider{}
		lp := &stubLoggerProvider{}

		x := otx.New(
			otx.WithTracerProvider(tp),
			otx.WithMeterProvider(mp),
			otx.WithLoggerProvider(lp),
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v9.9.9"),
			otx.WithScopeSchemaURL("https://example.test/schema"),
		)

		// The instrument stored is the very value the provider handed out.
		require.Same(t, tp.tracer, x.Tracer())
		require.Same(t, mp.meter, x.Meter())
		require.Same(t, lp.logger, x.Logger())

		// And it was asked for with the configured scope.
		for _, tc := range []struct {
			name    string
			scope   string
			version string
			schema  string
		}{
			{"tracer", tp.scope_name, tp.scope_cfg.InstrumentationVersion(), tp.scope_cfg.SchemaURL()},
			{"meter", mp.scope_name, mp.scope_cfg.InstrumentationVersion(), mp.scope_cfg.SchemaURL()},
			{"logger", lp.scope_name, lp.scope_cfg.InstrumentationVersion(), lp.scope_cfg.SchemaURL()},
		} {
			require.Equal(t, "app", tc.scope, tc.name)
			require.Equal(t, "v9.9.9", tc.version, tc.name)
			require.Equal(t, "https://example.test/schema", tc.schema, tc.name)
		}
	})
	t.Run("the tracer is the one the provider caches for the scope", func(t *testing.T) {
		tp := sdktrace.NewTracerProvider()
		x := otx.New(
			otx.WithTracerProvider(tp),
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v1.2.3"),
			otx.WithScopeSchemaURL("https://example.test/schema"),
		)

		require.Same(t, tp.Tracer(
			"app",
			trace.WithInstrumentationVersion("v1.2.3"),
			trace.WithSchemaURL("https://example.test/schema"),
		), x.Tracer())
	})
	t.Run("the logger is the one the provider caches for the scope", func(t *testing.T) {
		lp := sdklog.NewLoggerProvider()
		x := otx.New(
			otx.WithLoggerProvider(lp),
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v1.2.3"),
		)

		require.Same(t, lp.Logger(
			"app",
			otellog.WithInstrumentationVersion("v1.2.3"),
		), x.Logger())
	})
	t.Run("the slog handler is built once", func(t *testing.T) {
		x := otx.New()
		require.NotNil(t, x.SlogHandler())
		require.Same(t, x.SlogHandler(), x.SlogHandler())
	})
}

func TestPropagator(t *testing.T) {
	t.Run("the default carries trace context and baggage", func(t *testing.T) {
		x := otx.New()
		require.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, x.Propagator().Fields())
	})
	t.Run("the default injects a traceparent header", func(t *testing.T) {
		sc := sampledSpanContext()
		ctx := trace.ContextWithSpanContext(t.Context(), sc)

		b, err := baggage.Parse("tenant=acme")
		require.NoError(t, err)
		ctx = baggage.ContextWithBaggage(ctx, b)

		carrier := propagation.HeaderCarrier{}
		otx.New().Propagator().Inject(ctx, carrier)

		require.Equal(t, "00-"+sc.TraceID().String()+"-"+sc.SpanID().String()+"-01", carrier.Get("traceparent"))
		require.Equal(t, "tenant=acme", carrier.Get("baggage"))
	})
	t.Run("unlike the otel global default which propagates nothing", func(t *testing.T) {
		// Documents why New does not simply defer to otel.GetTextMapPropagator:
		// nothing installs a global propagator here, and its zero value is an
		// empty composite that silently drops the trace context.
		ctx := trace.ContextWithSpanContext(t.Context(), sampledSpanContext())

		carrier := propagation.HeaderCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)

		require.Empty(t, otel.GetTextMapPropagator().Fields())
		require.Empty(t, carrier)
	})
	t.Run("extract round trips through the default", func(t *testing.T) {
		sc := sampledSpanContext()
		p := otx.New().Propagator()

		carrier := propagation.HeaderCarrier{}
		p.Inject(trace.ContextWithSpanContext(t.Context(), sc), carrier)

		got := trace.SpanContextFromContext(p.Extract(t.Context(), carrier))
		require.Equal(t, sc.TraceID(), got.TraceID())
		require.Equal(t, sc.SpanID(), got.SpanID())
		require.True(t, got.IsSampled())
		require.True(t, got.IsRemote())
	})
}

func TestInto(t *testing.T) {
	t.Run("round trips", func(t *testing.T) {
		x := otx.New()
		ctx := otx.Into(t.Context(), x)

		require.Same(t, x, otx.From(ctx))

		v, ok := otx.FromOK(ctx)
		require.True(t, ok)
		require.Same(t, x, v)
	})
	t.Run("a nil Otx leaves the context unchanged", func(t *testing.T) {
		x := otx.New()
		ctx := otx.Into(t.Context(), x)

		// A mis-wired call must not poison a context that already carries one.
		require.Same(t, ctx, otx.Into(ctx, nil))
		require.Same(t, x, otx.From(otx.Into(ctx, nil)))

		// Nor make From panic on one that does not.
		bare := t.Context()
		require.Same(t, bare, otx.Into(bare, nil))

		_, ok := otx.FromOK(otx.Into(bare, nil))
		require.False(t, ok)
		require.NotNil(t, otx.From(otx.Into(bare, nil)))
	})
	t.Run("the innermost value wins", func(t *testing.T) {
		a := otx.New()
		b := otx.New()
		require.NotSame(t, a, b)

		ctx := otx.Into(otx.Into(t.Context(), a), b)
		require.Same(t, b, otx.From(ctx))
	})
}

func TestFrom(t *testing.T) {
	t.Run("a bare context yields a usable fallback", func(t *testing.T) {
		x := otx.From(t.Context())
		require.NotNil(t, x)
		require.NotNil(t, x.Tracer())
		require.NotNil(t, x.Meter())
		require.NotNil(t, x.Logger())
		require.NotNil(t, x.SlogHandler())
		require.NotNil(t, x.Propagator())
	})
	t.Run("the fallback is the same value on every call", func(t *testing.T) {
		// Regression: From used to fabricate a fresh Otx per call, so nothing
		// derived from it could be cached or compared.
		a := otx.From(t.Context())
		b := otx.From(context.Background())
		c := otx.From(otx.Into(t.Context(), nil))

		require.Same(t, a, b)
		require.Same(t, a, c)
	})
	t.Run("the fallback is the same value across goroutines", func(t *testing.T) {
		var (
			wg sync.WaitGroup
			mu sync.Mutex
			vs []*otx.Otx
		)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()

				v := otx.From(context.Background())

				mu.Lock()
				defer mu.Unlock()
				vs = append(vs, v)
			}()
		}
		wg.Wait()

		require.Len(t, vs, 8)
		for _, v := range vs {
			require.Same(t, vs[0], v)
		}
	})
	t.Run("FromOK reports false on a bare context", func(t *testing.T) {
		v, ok := otx.FromOK(t.Context())
		require.False(t, ok)
		require.Nil(t, v)
	})
}

func TestAccessors(t *testing.T) {
	tp := &stubTracerProvider{}
	mp := &stubMeterProvider{}
	lp := &stubLoggerProvider{}
	pr := &stubPropagator{fields: []string{"stub"}}

	x := otx.New(
		otx.WithTracerProvider(tp),
		otx.WithMeterProvider(mp),
		otx.WithLoggerProvider(lp),
		otx.WithPropagator(pr),
	)
	ctx := otx.Into(t.Context(), x)

	t.Run("the package level form delegates to the carried Otx", func(t *testing.T) {
		require.Same(t, x.Tracer(), otx.Tracer(ctx))
		require.Same(t, x.Meter(), otx.Meter(ctx))
		require.Same(t, x.Logger(), otx.Logger(ctx))
		require.Same(t, x.Propagator(), otx.Propagator(ctx))

		require.Same(t, x.Providers().Tracer(), otx.Providers(ctx).Tracer())
		require.Same(t, x.Providers().Meter(), otx.Providers(ctx).Meter())
		require.Same(t, x.Providers().Logger(), otx.Providers(ctx).Logger())
	})
	t.Run("the provider set holds what was configured", func(t *testing.T) {
		require.Same(t, tp, x.Providers().Tracer())
		require.Same(t, mp, x.Providers().Meter())
		require.Same(t, lp, x.Providers().Logger())
		require.Same(t, pr, x.Propagator())
	})
	t.Run("the package level form falls back on a bare context", func(t *testing.T) {
		bare := t.Context()
		f := otx.From(bare)

		require.Same(t, f.Tracer(), otx.Tracer(bare))
		require.Same(t, f.Meter(), otx.Meter(bare))
		require.Same(t, f.Logger(), otx.Logger(bare))
		require.Equal(t, f.Propagator(), otx.Propagator(bare))
		require.Same(t, f.Providers().Tracer(), otx.Providers(bare).Tracer())
	})
}

func TestTraceStart(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	x := otx.New(otx.WithTracerProvider(tp), otx.WithScopeName("app"), otx.WithScopeVersion("v0.1.0"))
	ctx := otx.Into(t.Context(), x)

	t.Run("starts a span on the tracer of the Otx", func(t *testing.T) {
		ctx_child, span := x.TraceStart(ctx, "method")
		require.Same(t, span, trace.SpanFromContext(ctx_child))
		require.True(t, span.IsRecording())
		span.End()

		ended := sr.Ended()
		require.Len(t, ended, 1)
		require.Equal(t, "method", ended[0].Name())
		require.Equal(t, "app", ended[0].InstrumentationScope().Name)
		require.Equal(t, "v0.1.0", ended[0].InstrumentationScope().Version)
	})
	t.Run("the package level form starts on the carried Otx", func(t *testing.T) {
		sr.Reset()

		ctx_child, span := otx.TraceStart(ctx, "package", trace.WithSpanKind(trace.SpanKindServer))
		require.Same(t, span, trace.SpanFromContext(ctx_child))
		span.End()

		ended := sr.Ended()
		require.Len(t, ended, 1)
		require.Equal(t, "package", ended[0].Name())
		require.Equal(t, trace.SpanKindServer, ended[0].SpanKind())
		require.Equal(t, "app", ended[0].InstrumentationScope().Name)
	})
	t.Run("nests under the span already in the context", func(t *testing.T) {
		sr.Reset()

		ctx_parent, parent := x.TraceStart(ctx, "parent")
		_, child := x.TraceStart(ctx_parent, "child")
		child.End()
		parent.End()

		require.Equal(t, parent.SpanContext().SpanID(), sr.Ended()[0].Parent().SpanID())
		require.Equal(t, parent.SpanContext().TraceID(), child.SpanContext().TraceID())
	})
	t.Run("does not panic on a bare context", func(t *testing.T) {
		require.NotPanics(t, func() {
			_, span := otx.TraceStart(t.Context(), "bare")
			span.End()
		})
	})
}

func TestSlogHandler(t *testing.T) {
	exporter := &otxmem.LogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))

	x := otx.New(otx.WithLoggerProvider(lp), otx.WithScopeName("app"), otx.WithScopeVersion("v3"))

	t.Run("bridges into the logger provider", func(t *testing.T) {
		l := slog.New(x.SlogHandler())
		l.InfoContext(t.Context(), "hello")

		require.Equal(t, 1, exporter.Len())

		record := exporter.Records()[0]
		require.Equal(t, "hello", record.Body().AsString())
		require.Equal(t, "app", record.InstrumentationScope().Name)
		require.Equal(t, "v3", record.InstrumentationScope().Version)
	})
	t.Run("the otel logger writes to the same provider", func(t *testing.T) {
		exporter.Reset()

		x.Logger().Emit(t.Context(), logRecord("direct"))

		require.Equal(t, 1, exporter.Len())
		require.Equal(t, "direct", exporter.Records()[0].Body().AsString())
		require.Equal(t, "app", exporter.Records()[0].InstrumentationScope().Name)
	})
}
