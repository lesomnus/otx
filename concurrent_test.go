package otx_test

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestConcurrentUse exercises the claim that an Otx is immutable once New
// returns and is safe for concurrent use. It is worth little without -race and
// a great deal with it: an Otx that derived an instrument lazily, or cached one
// on first use, would be caught here and nowhere else, because every other test
// in this package drives it from a single goroutine.
//
// The goroutines only collect what they observed; the assertions are made on
// the test goroutine afterwards, since testify's require stops the goroutine it
// runs on rather than the test.
func TestConcurrentUse(t *testing.T) {
	const n = 16

	spans := tracetest.NewSpanRecorder()
	logs := &otxmem.LogExporter{}

	x := otx.New(
		otx.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))),
		otx.WithLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))),
		otx.WithScopeName("app"),
	)
	ctx := otx.Into(t.Context(), x)

	// fan runs body n times in parallel, once per index.
	fan := func(body func(i int)) {
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body(i)
			}()
		}
		wg.Wait()
	}

	t.Run("every goroutine sees the same instruments", func(t *testing.T) {
		type seen struct {
			otx      *otx.Otx
			tracer   trace.Tracer
			meter    metric.Meter
			logger   otellog.Logger
			handler  slog.Handler
			provider trace.TracerProvider
		}

		got := make([]seen, n)
		fan(func(i int) {
			got[i] = seen{
				otx:      otx.From(ctx),
				tracer:   otx.Tracer(ctx),
				meter:    otx.Meter(ctx),
				logger:   otx.Logger(ctx),
				handler:  otx.From(ctx).SlogHandler(),
				provider: otx.Providers(ctx).Tracer(),
			}
		})

		for i, v := range got {
			require.Same(t, x, v.otx, i)
			require.Same(t, x.Tracer(), v.tracer, i)
			require.Same(t, x.Meter(), v.meter, i)
			require.Same(t, x.Logger(), v.logger, i)
			require.Same(t, x.SlogHandler(), v.handler, i)
			require.Same(t, x.Providers().Tracer(), v.provider, i)
		}
	})
	t.Run("emitting from many goroutines loses nothing", func(t *testing.T) {
		fan(func(i int) {
			_, span := otx.TraceStart(ctx, "op")
			span.End()

			otx.Logger(ctx).Emit(ctx, logRecord("emitted"))
			slog.New(x.SlogHandler()).InfoContext(ctx, "bridged")
		})

		require.Len(t, spans.Ended(), n)
		require.Equal(t, 2*n, logs.Len())
	})
	t.Run("flushing runs alongside emitting", func(t *testing.T) {
		errs := make([]error, n)
		fan(func(i int) {
			_, span := otx.TraceStart(ctx, "op")
			span.End()

			errs[i] = otx.ForceFlush(ctx)
		})

		for i, err := range errs {
			require.NoError(t, err, i)
		}
	})
	t.Run("shutdown runs once however many goroutines call it", func(t *testing.T) {
		// Runs last: it closes the providers the subtests above share.
		errs := make([]error, n)
		fan(func(i int) {
			errs[i] = otx.Shutdown(ctx)
		})

		for i, err := range errs {
			require.NoError(t, err, i)
		}

		before := spans.Ended()

		_, span := otx.TraceStart(ctx, "after shutdown")
		span.End()
		require.Len(t, spans.Ended(), len(before), "the tracer provider was closed exactly once, by one of them")
	})
}
