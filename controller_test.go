package otx_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	errController = errors.New("controller failed")
	errTracer     = errors.New("tracer provider failed")
	errMeter      = errors.New("meter provider failed")
	errLogger     = errors.New("logger provider failed")
)

func TestLifecycle(t *testing.T) {
	ctx := t.Context()

	t.Run("without a controller start and shutdown are no-ops", func(t *testing.T) {
		x := otx.New()
		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))
	})
	t.Run("start reaches the controller exactly once", func(t *testing.T) {
		c := &stubController{name: "c"}
		x := otx.New(otx.WithController(c))

		require.NoError(t, x.Start(ctx))

		require.Equal(t, 1, c.start_n)
		require.Zero(t, c.shutdown_n)
	})
	t.Run("shutdown reaches the controller exactly once and never calls start", func(t *testing.T) {
		// Regression: Otx.Shutdown used to call Controller.Start, a copy-paste
		// slip that shipped in every release before the rewrite.
		c := &stubController{name: "c"}
		x := otx.New(otx.WithController(c))

		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, c.shutdown_n)
		require.Zero(t, c.start_n, "Shutdown must not call Start")
	})
	t.Run("start and shutdown run in that order", func(t *testing.T) {
		order := []string{}
		c := &stubController{name: "c", trace: &order}
		x := otx.New(otx.WithController(c))

		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, []string{"start:c", "shutdown:c"}, order)
	})
	t.Run("start propagates the controller error", func(t *testing.T) {
		c := &stubController{name: "c", start_err: errController}
		x := otx.New(otx.WithController(c))

		require.ErrorIs(t, x.Start(ctx), errController)
	})
	t.Run("the package level form delegates", func(t *testing.T) {
		c := &flushController{stubController: stubController{name: "c"}}
		x := otx.New(otx.WithController(c))
		ctx_otx := otx.Into(ctx, x)

		require.NoError(t, otx.Start(ctx_otx))
		require.NoError(t, otx.ForceFlush(ctx_otx))
		require.NoError(t, otx.Shutdown(ctx_otx))

		require.Equal(t, 1, c.start_n)
		require.Equal(t, 1, c.flush_n)
		require.Equal(t, 1, c.shutdown_n)
	})
	t.Run("the package level form reports a context with no Otx", func(t *testing.T) {
		// The accessors degrade to the globals, but reporting a successful
		// shutdown of nothing would hide a wiring mistake.
		bare := t.Context()

		require.ErrorIs(t, otx.Start(bare), otx.ErrNoOtx)
		require.ErrorIs(t, otx.Shutdown(bare), otx.ErrNoOtx)
		require.ErrorIs(t, otx.ForceFlush(bare), otx.ErrNoOtx)

		// Into with a nil Otx does not install one either.
		nil_ctx := otx.Into(bare, nil)
		require.ErrorIs(t, otx.Start(nil_ctx), otx.ErrNoOtx)
		require.ErrorIs(t, otx.Shutdown(nil_ctx), otx.ErrNoOtx)
		require.ErrorIs(t, otx.ForceFlush(nil_ctx), otx.ErrNoOtx)
	})
}

func TestShutdownProviders(t *testing.T) {
	ctx := t.Context()

	t.Run("shuts down the providers it was given", func(t *testing.T) {
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

		exporter := &otxmem.LogExporter{}
		lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))

		x := otx.New(otx.WithTracerProvider(tp), otx.WithLoggerProvider(lp))

		_, span := x.TraceStart(ctx, "before")
		span.End()
		x.Logger().Emit(ctx, logRecord("before"))
		require.Len(t, sr.Ended(), 1)
		require.Equal(t, 1, exporter.Len())

		require.True(t, lp.Logger("probe").Enabled(ctx, otellog.EnabledParameters{}))

		require.NoError(t, x.Shutdown(ctx))

		sr.Reset()

		_, span = x.TraceStart(ctx, "after")
		span.End()
		require.Empty(t, sr.Ended(), "the tracer provider is shut down")

		// The providers themselves report it: once shut down they hand out
		// non-recording tracers and disabled loggers.
		_, probe := tp.Tracer("probe").Start(ctx, "probe")
		require.False(t, probe.IsRecording(), "the tracer provider is shut down")
		require.False(t, lp.Logger("probe").Enabled(ctx, otellog.EnabledParameters{}), "the logger provider is shut down")
	})
	t.Run("a provider with no lifecycle methods is left alone", func(t *testing.T) {
		x := otx.New(otx.WithTracerProvider(&plainTracerProvider{}))

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))
	})
	t.Run("joins the controller and every provider error", func(t *testing.T) {
		c := &stubController{name: "c", shutdown_err: errController}
		tp := &stubTracerProvider{shutdown_err: errTracer}
		mp := &stubMeterProvider{shutdown_err: errMeter}
		lp := &stubLoggerProvider{shutdown_err: errLogger}

		x := otx.New(
			otx.WithController(c),
			otx.WithTracerProvider(tp),
			otx.WithMeterProvider(mp),
			otx.WithLoggerProvider(lp),
		)

		err := x.Shutdown(ctx)
		require.ErrorIs(t, err, errController)
		require.ErrorIs(t, err, errTracer)
		require.ErrorIs(t, err, errMeter)
		require.ErrorIs(t, err, errLogger)
	})
	t.Run("is idempotent", func(t *testing.T) {
		c := &stubController{name: "c", shutdown_err: errController}
		tp := &stubTracerProvider{shutdown_err: errTracer}

		x := otx.New(otx.WithController(c), otx.WithTracerProvider(tp))

		err := x.Shutdown(ctx)
		require.Error(t, err)

		require.Same(t, err, x.Shutdown(ctx), "the second call returns the result of the first")
		require.Same(t, err, x.Shutdown(ctx))

		require.Equal(t, 1, c.shutdown_n)
		require.Equal(t, 1, tp.shutdown_n)
	})
	t.Run("is safe to call from several goroutines", func(t *testing.T) {
		c := &stubController{name: "c", shutdown_err: errController}
		tp := &stubTracerProvider{shutdown_err: errTracer}

		x := otx.New(otx.WithController(c), otx.WithTracerProvider(tp))

		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs []error
		)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()

				err := x.Shutdown(ctx)

				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, err)
			}()
		}
		wg.Wait()

		require.Len(t, errs, 8)
		for _, err := range errs {
			require.Same(t, errs[0], err)
			require.ErrorIs(t, err, errController)
			require.ErrorIs(t, err, errTracer)
		}

		require.Equal(t, 1, c.shutdown_n)
		require.Equal(t, 1, tp.shutdown_n)
	})
	t.Run("a propagator is never shut down however closable it looks", func(t *testing.T) {
		// Ownership follows the option, not the shape of the value: only the
		// three provider options hand a value to own. A propagator that
		// happens to carry Shutdown and ForceFlush must be left alone.
		pr := &closablePropagator{stubPropagator: stubPropagator{fields: []string{"stub"}}}
		tp := &stubTracerProvider{}

		x := otx.New(otx.WithPropagator(pr), otx.WithTracerProvider(tp), otx.WithScopeName("app"))
		require.Same(t, pr, x.Propagator())

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		// The provider given through its own option was, so the Otx really did
		// run its lifecycle rather than doing nothing at all.
		require.Equal(t, 1, tp.flush_n)
		require.Equal(t, 1, tp.shutdown_n)

		require.Zero(t, pr.shutdown_n, "WithPropagator takes no ownership")
		require.Zero(t, pr.flush_n, "WithPropagator takes no ownership")
	})
	t.Run("shuts down the providers in signal order and then the controller", func(t *testing.T) {
		order := []string{}
		c := &stubController{name: "controller", trace: &order}
		tp := &stubTracerProvider{lifecycleTrace: lifecycleTrace{name: "tracer", trace: &order}}
		mp := &stubMeterProvider{lifecycleTrace: lifecycleTrace{name: "meter", trace: &order}}
		lp := &stubLoggerProvider{lifecycleTrace: lifecycleTrace{name: "logger", trace: &order}}

		// Deliberately given out of signal order: ownership is indexed per
		// signal, so the shutdown order must not follow the option order.
		x := otx.New(
			otx.WithLoggerProvider(lp),
			otx.WithMeterProvider(mp),
			otx.WithTracerProvider(tp),
			otx.WithController(c),
		)

		require.NoError(t, x.Shutdown(ctx))
		require.Equal(t, []string{
			"shutdown:tracer",
			"shutdown:meter",
			"shutdown:logger",
			"shutdown:controller",
		}, order)
	})
	t.Run("a provider flushes before the controller closes what it flushes into", func(t *testing.T) {
		// The reason for that order, as the failure it prevents. A Controller
		// that owns the exporter - which is what a resolver built from a
		// configuration file gives you - would otherwise close it before the
		// batch processor gets to flush, and the batch would be dropped
		// without Shutdown reporting anything.
		exporter := &recordingSpanExporter{}
		tracer_provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))

		x := otx.New(
			otx.WithTracerProvider(tracer_provider),
			otx.WithController(otx.NewController(nil, exporter.Shutdown)),
		)

		_, span := x.TraceStart(ctx, "work")
		span.End()
		require.Zero(t, exporter.Exported(), "a batch processor exports nothing until it is flushed")

		require.NoError(t, x.Shutdown(ctx))
		require.Equal(t, 1, exporter.Exported())
		require.False(t, exporter.ExportedLate(), "the span was flushed into an exporter that was already shut down")
	})
	t.Run("a provider given to two signals is shut down once", func(t *testing.T) {
		// One object can serve more than one signal. Its hooks are registered
		// once, so a single Shutdown does not shut it down once per signal.
		// Observable only through a counting stub, since both SDK providers are
		// idempotent on Shutdown.
		p := &dualProvider{}

		x := otx.New(otx.WithTracerProvider(p), otx.WithLoggerProvider(p))
		require.Same(t, p, x.Providers().Tracer())
		require.Same(t, p, x.Providers().Logger())

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, p.shutdown_n)
		require.Equal(t, 1, p.flush_n)
	})
	t.Run("a provider that cannot be compared is still shut down", func(t *testing.T) {
		// The identity check that de-duplicates a provider given to two
		// signals cannot compare every type. When it cannot, it must fall back
		// to registering the hooks rather than dropping them, so the provider
		// is still shut down - at worst once per signal.
		n := 0
		p := uncomparableProvider{shutdown_n: &n}

		x := otx.New(otx.WithTracerProvider(p), otx.WithLoggerProvider(p))
		require.NoError(t, x.Shutdown(ctx))
		require.GreaterOrEqual(t, n, 1)
	})
	t.Run("two distinct providers are each shut down once", func(t *testing.T) {
		// The counterpart of the case above: de-duplicating by identity must
		// not collapse two different providers of the same type.
		a, b := &dualProvider{}, &dualProvider{}

		x := otx.New(otx.WithTracerProvider(a), otx.WithLoggerProvider(b))
		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.Shutdown(ctx))

		require.Equal(t, 1, a.shutdown_n)
		require.Equal(t, 1, b.shutdown_n)
		require.Equal(t, 1, a.flush_n)
		require.Equal(t, 1, b.flush_n)
	})
	t.Run("flushing after shutdown still reaches the providers", func(t *testing.T) {
		// Otx keeps no closed flag of its own: the flush hooks stay callable
		// and it is the provider's job to refuse. Pinned so that adding a
		// guard is a deliberate change rather than an accident.
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithTracerProvider(tp))

		require.NoError(t, x.Shutdown(ctx))
		require.NoError(t, x.ForceFlush(ctx))

		require.Equal(t, 1, tp.shutdown_n)
		require.Equal(t, 1, tp.flush_n)
	})
}

func TestForceFlush(t *testing.T) {
	ctx := t.Context()

	t.Run("flushes every owned provider", func(t *testing.T) {
		tp := &stubTracerProvider{}
		mp := &stubMeterProvider{}
		lp := &stubLoggerProvider{}

		x := otx.New(
			otx.WithTracerProvider(tp),
			otx.WithMeterProvider(mp),
			otx.WithLoggerProvider(lp),
		)

		require.NoError(t, x.ForceFlush(ctx))

		require.Equal(t, 1, tp.flush_n)
		require.Equal(t, 1, mp.flush_n)
		require.Equal(t, 1, lp.flush_n)

		require.Zero(t, tp.shutdown_n, "ForceFlush must not shut anything down")
		require.Zero(t, mp.shutdown_n)
		require.Zero(t, lp.shutdown_n)
	})
	t.Run("flushes the controller when it has a ForceFlush", func(t *testing.T) {
		order := []string{}
		c := &flushController{stubController: stubController{name: "c", trace: &order}}
		x := otx.New(otx.WithController(c))

		require.NoError(t, x.ForceFlush(ctx))

		require.Equal(t, 1, c.flush_n)
		require.Equal(t, []string{"flush:c"}, order)
		require.Zero(t, c.start_n)
		require.Zero(t, c.shutdown_n)
	})
	t.Run("skips a controller without one", func(t *testing.T) {
		c := &stubController{name: "c"}
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithController(c), otx.WithTracerProvider(tp))

		require.NotPanics(t, func() {
			require.NoError(t, x.ForceFlush(ctx))
		})

		require.Zero(t, c.start_n)
		require.Zero(t, c.shutdown_n)
		require.Equal(t, 1, tp.flush_n)
	})
	t.Run("joins the controller and every provider error", func(t *testing.T) {
		c := &flushController{stubController: stubController{name: "c"}, flush_err: errController}
		tp := &stubTracerProvider{flush_err: errTracer}
		mp := &stubMeterProvider{flush_err: errMeter}
		lp := &stubLoggerProvider{flush_err: errLogger}

		x := otx.New(
			otx.WithController(c),
			otx.WithTracerProvider(tp),
			otx.WithMeterProvider(mp),
			otx.WithLoggerProvider(lp),
		)

		err := x.ForceFlush(ctx)
		require.ErrorIs(t, err, errController)
		require.ErrorIs(t, err, errTracer)
		require.ErrorIs(t, err, errMeter)
		require.ErrorIs(t, err, errLogger)
	})
	t.Run("can be called more than once", func(t *testing.T) {
		tp := &stubTracerProvider{}
		x := otx.New(otx.WithTracerProvider(tp))

		require.NoError(t, x.ForceFlush(ctx))
		require.NoError(t, x.ForceFlush(ctx))

		require.Equal(t, 2, tp.flush_n)
	})
	t.Run("flushes the controller first and then the providers in signal order", func(t *testing.T) {
		order := []string{}
		c := &flushController{stubController: stubController{name: "controller", trace: &order}}
		tp := &stubTracerProvider{lifecycleTrace: lifecycleTrace{name: "tracer", trace: &order}}
		mp := &stubMeterProvider{lifecycleTrace: lifecycleTrace{name: "meter", trace: &order}}
		lp := &stubLoggerProvider{lifecycleTrace: lifecycleTrace{name: "logger", trace: &order}}

		x := otx.New(
			otx.WithLoggerProvider(lp),
			otx.WithMeterProvider(mp),
			otx.WithTracerProvider(tp),
			otx.WithController(c),
		)

		require.NoError(t, x.ForceFlush(ctx))
		require.Equal(t, []string{
			"flush:controller",
			"flush:tracer",
			"flush:meter",
			"flush:logger",
		}, order)
	})
}

func TestNewController(t *testing.T) {
	ctx := t.Context()

	t.Run("adapts a pair of functions", func(t *testing.T) {
		order := []string{}
		c := otx.NewController(
			func(ctx context.Context) error {
				order = append(order, "start")
				return nil
			},
			func(ctx context.Context) error {
				order = append(order, "shutdown")
				return errController
			},
		)

		require.NoError(t, c.Start(ctx))
		require.ErrorIs(t, c.Shutdown(ctx), errController)
		require.Equal(t, []string{"start", "shutdown"}, order)
	})
	t.Run("passes the context through", func(t *testing.T) {
		type key struct{}
		want := context.WithValue(ctx, key{}, "v")

		var got_start, got_shutdown context.Context
		c := otx.NewController(
			func(ctx context.Context) error { got_start = ctx; return nil },
			func(ctx context.Context) error { got_shutdown = ctx; return nil },
		)

		require.NoError(t, c.Start(want))
		require.NoError(t, c.Shutdown(want))
		require.Same(t, want, got_start)
		require.Same(t, want, got_shutdown)
	})
	t.Run("a nil start is a no-op", func(t *testing.T) {
		called := false
		c := otx.NewController(nil, func(ctx context.Context) error { called = true; return nil })

		require.NoError(t, c.Start(ctx))
		require.NoError(t, c.Shutdown(ctx))
		require.True(t, called)
	})
	t.Run("a nil shutdown is a no-op", func(t *testing.T) {
		called := false
		c := otx.NewController(func(ctx context.Context) error { called = true; return nil }, nil)

		require.NoError(t, c.Start(ctx))
		require.NoError(t, c.Shutdown(ctx))
		require.True(t, called)
	})
	t.Run("both nil is a no-op both ways", func(t *testing.T) {
		c := otx.NewController(nil, nil)

		require.NoError(t, c.Start(ctx))
		require.NoError(t, c.Shutdown(ctx))
	})
	t.Run("drives an Otx", func(t *testing.T) {
		n_start, n_shutdown := 0, 0
		x := otx.New(otx.WithController(otx.NewController(
			func(ctx context.Context) error { n_start++; return nil },
			func(ctx context.Context) error { n_shutdown++; return nil },
		)))

		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))
		require.Equal(t, 1, n_start)
		require.Equal(t, 1, n_shutdown)
	})
}

func TestJoinControllers(t *testing.T) {
	ctx := t.Context()

	t.Run("starts in order and shuts down in reverse", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order}
		b := &stubController{name: "b", trace: &order}
		c := &stubController{name: "c", trace: &order}

		joined := otx.JoinControllers(a, b, c)

		require.NoError(t, joined.Start(ctx))
		require.Equal(t, []string{"start:a", "start:b", "start:c"}, order)

		order = order[:0]
		require.NoError(t, joined.Shutdown(ctx))
		require.Equal(t, []string{"shutdown:c", "shutdown:b", "shutdown:a"}, order)
	})
	t.Run("a failed start unwinds what it already started", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order}
		b := &stubController{name: "b", trace: &order}
		c := &stubController{name: "c", trace: &order, start_err: errController}
		d := &stubController{name: "d", trace: &order}

		err := otx.JoinControllers(a, b, c, d).Start(ctx)
		require.ErrorIs(t, err, errController)

		require.Equal(t, []string{
			"start:a", "start:b", "start:c",
			"shutdown:b", "shutdown:a",
		}, order)

		require.Zero(t, d.start_n, "the controllers after the failure are never started")
		require.Zero(t, c.shutdown_n, "a controller that failed to start cleans up after itself")
	})
	t.Run("a failed unwind joins its error too", func(t *testing.T) {
		a := &stubController{name: "a", shutdown_err: errTracer}
		b := &stubController{name: "b", start_err: errController}

		err := otx.JoinControllers(a, b).Start(ctx)
		require.ErrorIs(t, err, errController)
		require.ErrorIs(t, err, errTracer)
	})
	t.Run("the first controller failing unwinds nothing", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order, start_err: errController}
		b := &stubController{name: "b", trace: &order}

		require.ErrorIs(t, otx.JoinControllers(a, b).Start(ctx), errController)
		require.Equal(t, []string{"start:a"}, order)
		require.Zero(t, a.shutdown_n)
	})
	t.Run("shutdown joins every error and still visits every controller", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order, shutdown_err: errTracer}
		b := &stubController{name: "b", trace: &order, shutdown_err: errMeter}
		c := &stubController{name: "c", trace: &order, shutdown_err: errLogger}

		err := otx.JoinControllers(a, b, c).Shutdown(ctx)
		require.ErrorIs(t, err, errTracer)
		require.ErrorIs(t, err, errMeter)
		require.ErrorIs(t, err, errLogger)
		require.Equal(t, []string{"shutdown:c", "shutdown:b", "shutdown:a"}, order)
	})
	t.Run("nil entries are ignored", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order}

		joined := otx.JoinControllers(nil, a, nil)
		require.NoError(t, joined.Start(ctx))
		require.NoError(t, joined.Shutdown(ctx))
		require.Equal(t, []string{"start:a", "shutdown:a"}, order)
	})
	t.Run("a typed nil entry is ignored", func(t *testing.T) {
		// An interface holding a nil *stubController is not itself nil, so a
		// `c != nil` filter would let it through and it would panic on the
		// first Start. JoinControllers screens it out the same way the options
		// do, so the two agree.
		var typed_nil *stubController

		joined := otx.JoinControllers(typed_nil)
		require.NoError(t, joined.Start(ctx))
		require.NoError(t, joined.Shutdown(ctx))

		// And through an Otx, where the joined slice itself is never nil.
		x := otx.New(otx.WithController(otx.JoinControllers(typed_nil)))
		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))

		// The same value handed straight to WithController is ignored too.
		var y *otx.Otx
		require.NotPanics(t, func() { y = otx.New(otx.WithController(typed_nil)) })
		require.NoError(t, y.Start(ctx))
		require.NoError(t, y.Shutdown(ctx))
	})
	t.Run("an empty join is a no-op and is still usable as a controller", func(t *testing.T) {
		joined := otx.JoinControllers()
		require.NoError(t, joined.Start(ctx))
		require.NoError(t, joined.Shutdown(ctx))

		x := otx.New(otx.WithController(joined))
		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))
	})
	t.Run("drives an Otx", func(t *testing.T) {
		order := []string{}
		a := &stubController{name: "a", trace: &order}
		b := &stubController{name: "b", trace: &order}

		x := otx.New(otx.WithController(otx.JoinControllers(a, b)))

		require.NoError(t, x.Start(ctx))
		require.NoError(t, x.Shutdown(ctx))
		require.Equal(t, []string{"start:a", "start:b", "shutdown:b", "shutdown:a"}, order)
	})
}
