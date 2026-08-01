package otxtest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxmem"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// fakeTB is a [testing.TB] that records what otxtest asks of it instead of
// failing the test that is running. The harness reports its own failures
// through the TB it was given, so those paths - the cleanup that could not shut
// the Otx down, and the Collect that could not read the reader - can only be
// exercised by handing New a TB under our control.
//
// It embeds testing.TB rather than implementing it, because testing.TB has an
// unexported method and cannot be satisfied any other way. The embedded value
// is nil on purpose: a method that otxtest starts calling and that is not
// overridden below panics with a nil dereference, which is a loud failure of
// this file rather than a silent hole in it.
//
// Every method is guarded by a mutex, because otxtest calls into the TB from
// whatever goroutine called the harness - Collect in particular - and a fake
// that appended to a slice unguarded would report a data race of its own making
// rather than one of otxtest's.
type fakeTB struct {
	testing.TB

	mu        sync.Mutex
	errors    []string
	fatals    []string
	cleanups  []func()
	helpers_n int
}

// fatal is what fakeTB.Fatalf panics with.
//
// The real [testing.T.Fatalf] marks the test failed and then ends the goroutine
// with runtime.Goexit, so nothing written after the call runs. A fake that only
// recorded the call would let the code under test carry on past the point the
// real one stops - Collect would return a zero ResourceMetrics as though
// nothing had gone wrong, and the caller would go on to assert on it. Panicking
// reproduces the unwind closely enough: deferred functions run, the rest of the
// function does not, and [recoverFatal] catches it at the boundary.
type fatal struct {
	msg string
}

// Helper counts the calls. A function that forgets to mark itself as a helper
// still behaves the same; only the file:line the testing package reports on a
// failure moves onto the helper itself, which no assertion on the outcome can
// see. Counting is the only way to pin it.
func (tb *fakeTB) Helper() {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.helpers_n++
}

func (tb *fakeTB) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.errors = append(tb.errors, msg)
}

func (tb *fakeTB) Fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)

	tb.mu.Lock()
	tb.fatals = append(tb.fatals, msg)
	tb.mu.Unlock()

	panic(fatal{msg: msg})
}

func (tb *fakeTB) Cleanup(f func()) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.cleanups = append(tb.cleanups, f)
}

// runCleanups runs the registered cleanups in reverse order of registration, as
// the testing package does, and forgets them so that a second call is a no-op.
func (tb *fakeTB) runCleanups() {
	tb.mu.Lock()
	fs := tb.cleanups
	tb.cleanups = nil
	tb.mu.Unlock()

	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

func (tb *fakeTB) snapshot() (errors []string, fatals []string, helpers_n int) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	return slices.Clone(tb.errors), slices.Clone(tb.fatals), tb.helpers_n
}

// recoverFatal runs f and reports whether it ended in a [fakeTB.Fatalf]. Any
// other panic is re-raised, so a genuine bug in f is not swallowed here.
func recoverFatal(t *testing.T, f func()) (is_fatal bool) {
	t.Helper()

	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if _, ok := v.(fatal); !ok {
			panic(v)
		}

		is_fatal = true
	}()

	f()

	return false
}

// failingProvider is a provider whose Shutdown reports an error. It is the only
// way to reach the failure branch of the cleanup New registers: every provider
// the harness builds itself shuts down cleanly.
//
// It serves all three signals so that a test can choose which recorder it
// displaces.
type failingProvider struct {
	tracenoop.TracerProvider
	lognoop.LoggerProvider

	err        error
	shutdown_n int
}

func (p *failingProvider) Shutdown(ctx context.Context) error {
	p.shutdown_n++

	return p.err
}

func namesOf(spans []sdktrace.ReadOnlySpan) []string {
	rst := make([]string, len(spans))
	for i := range spans {
		rst[i] = spans[i].Name()
	}

	return rst
}

func bodiesOf(records []sdklog.Record) []string {
	rst := make([]string, len(records))
	for i := range records {
		rst[i] = records[i].Body().AsString()
	}

	return rst
}

// sumOf returns the single data point of the int64 sum named name.
func sumOf(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			data, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %q holds %T, not a Sum[int64]", name, m.Data)
			require.Len(t, data.DataPoints, 1)

			return data.DataPoints[0].Value
		}
	}

	require.FailNow(t, "metric not found", "no metric named %q was collected", name)

	return 0
}

// emit produces one of every signal through the Otx carried by ctx, which is
// how a test under test would use the harness: through the context, not through
// the recorders.
func emit(ctx context.Context, name string) {
	_, span := otx.TraceStart(ctx, name)
	span.End()

	log.From(ctx).Info(name)
	recordHit(ctx, 1)
}

func TestNew(t *testing.T) {
	t.Run("wires the otx to all three recorders", func(t *testing.T) {
		h := otxtest.New(t)
		require.NotNil(t, h.Otx)
		require.NotNil(t, h.Spans)
		require.NotNil(t, h.Logs)
		require.NotNil(t, h.Reader)

		ctx := h.Into(t.Context())

		_, span := h.Otx.TraceStart(ctx, "op")
		span.End()
		log.From(ctx).Info("hello")
		recordHit(h.Into(ctx), 2)

		require.Equal(t, []string{"op"}, namesOf(h.Ended()))
		require.Equal(t, []string{"hello"}, bodiesOf(h.Records()))
		require.Equal(t, int64(2), sumOf(t, h.Collect(ctx), "hits"))
	})
	t.Run("the exported providers are the ones the otx uses", func(t *testing.T) {
		h := otxtest.New(t)

		ps := h.Otx.Providers()
		require.Same(t, h.TracerProvider, ps.Tracer())
		require.Same(t, h.MeterProvider, ps.Meter())
		require.Same(t, h.LoggerProvider, ps.Logger())
	})
	t.Run("the exported recorders are the ones behind the providers", func(t *testing.T) {
		// The providers are reachable through the fields as well, so a test
		// that needs something the methods do not offer can drive them
		// directly and still see the result through the recorders.
		h := otxtest.New(t)
		ctx := t.Context()

		_, span := h.TracerProvider.Tracer("direct").Start(ctx, "op")
		span.End()

		r := otellog.Record{}
		r.SetBody(otellog.StringValue("hello"))
		h.LoggerProvider.Logger("direct").Emit(ctx, r)

		counter, err := h.MeterProvider.Meter("direct").Int64Counter("hits")
		require.NoError(t, err)
		counter.Add(ctx, 3)

		require.Equal(t, []string{"op"}, namesOf(h.Spans.Ended()))
		require.Equal(t, []string{"hello"}, bodiesOf(h.Logs.Records()))

		rm := metricdata.ResourceMetrics{}
		require.NoError(t, h.Reader.Collect(ctx, &rm))
		require.Equal(t, int64(3), sumOf(t, rm, "hits"))
	})
	t.Run("options are applied after the recorders so an option wins", func(t *testing.T) {
		// This is the documented caveat: an option may replace a recorder, and
		// the field of the Harness then no longer reflects what the Otx uses.
		other_spans := tracetest.NewSpanRecorder()
		other := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(other_spans))

		h := otxtest.New(t, otx.WithTracerProvider(other))

		require.Same(t, other, h.Otx.Providers().Tracer())
		require.NotSame(t, h.TracerProvider, h.Otx.Providers().Tracer())

		ctx := h.Into(t.Context())
		_, span := otx.TraceStart(ctx, "op")
		span.End()

		require.Equal(t, []string{"op"}, namesOf(other_spans.Ended()))
		require.Empty(t, h.Ended(), "the displaced recorder sees nothing")
		require.Empty(t, h.Spans.Ended())

		// The field still holds the harness's own provider, still wired to the
		// harness's own recorder; it is only the Otx that no longer uses it.
		_, own := h.TracerProvider.Tracer("direct").Start(ctx, "direct")
		own.End()
		require.Equal(t, []string{"direct"}, namesOf(h.Ended()))

		// Only the signal the option named is displaced.
		require.Same(t, h.MeterProvider, h.Otx.Providers().Meter())
		require.Same(t, h.LoggerProvider, h.Otx.Providers().Logger())

		log.From(ctx).Info("hello")
		require.Equal(t, []string{"hello"}, bodiesOf(h.Records()))

		// The harness's own tracer provider is no longer owned by the Otx, so
		// the cleanup will not shut it down.
		t.Cleanup(func() { require.NoError(t, h.TracerProvider.Shutdown(context.Background())) })
	})
	t.Run("an option that displaces the meter provider leaves collect empty", func(t *testing.T) {
		// The same caveat for the pulled signal: Collect reads the reader of
		// the Harness, which the displaced provider no longer feeds.
		other_reader := sdkmetric.NewManualReader()
		other := sdkmetric.NewMeterProvider(sdkmetric.WithReader(other_reader))

		// The Otx owns "other" because it came through an option, so the
		// cleanup New registers shuts it down; only the displaced provider of
		// the Harness is left for this test to close.
		h := otxtest.New(t, otx.WithMeterProvider(other))
		ctx := h.Into(t.Context())

		recordHit(ctx, 1)

		require.Same(t, other, h.Otx.Providers().Meter())
		require.Empty(t, h.Collect(ctx).ScopeMetrics, "the harness reader is no longer fed")

		rm := metricdata.ResourceMetrics{}
		require.NoError(t, other_reader.Collect(ctx, &rm))
		require.Equal(t, int64(1), sumOf(t, rm, "hits"))

		// Collect is still wired to the harness's own reader, which the
		// harness's own provider still feeds; it is only the Otx that stopped
		// writing to it.
		counter, err := h.MeterProvider.Meter("direct").Int64Counter("misses")
		require.NoError(t, err)
		counter.Add(ctx, 7)
		require.Equal(t, int64(7), sumOf(t, h.Collect(ctx), "misses"))

		t.Cleanup(func() { require.NoError(t, h.MeterProvider.Shutdown(context.Background())) })
	})
	t.Run("other options pass through untouched", func(t *testing.T) {
		h := otxtest.New(t, otx.WithScopeName("app"))
		ctx := h.Into(t.Context())

		emit(ctx, "op")

		require.Len(t, h.Ended(), 1)
		require.Equal(t, "app", h.Ended()[0].InstrumentationScope().Name)

		require.Len(t, h.Records(), 1)
		require.Equal(t, "app", h.Records()[0].InstrumentationScope().Name)

		rm := h.Collect(ctx)
		require.Len(t, rm.ScopeMetrics, 1)
		require.Equal(t, "app", rm.ScopeMetrics[0].Scope.Name)
	})
	t.Run("a nil option is ignored", func(t *testing.T) {
		h := otxtest.New(t, nil, otx.WithScopeName("app"), nil)
		ctx := h.Into(t.Context())

		_, span := otx.TraceStart(ctx, "op")
		span.End()

		require.Len(t, h.Ended(), 1)
		require.Equal(t, "app", h.Ended()[0].InstrumentationScope().Name)
	})
	t.Run("each harness records on its own", func(t *testing.T) {
		a := otxtest.New(t)
		b := otxtest.New(t)

		emit(a.Into(t.Context()), "a")

		require.Equal(t, []string{"a"}, namesOf(a.Ended()))
		require.Empty(t, b.Ended())
		require.Empty(t, b.Records())
		require.Empty(t, b.Collect(t.Context()).ScopeMetrics)
	})
	t.Run("marks itself as a test helper", func(t *testing.T) {
		// So that a failure the harness reports - the shutdown below - points
		// at the line that called New rather than at a line inside otxtest.
		tb := &fakeTB{}
		otxtest.New(tb, otx.WithTracerProvider(&failingProvider{err: errors.New("boom")}))

		_, _, helpers_n := tb.snapshot()
		require.Equal(t, 1, helpers_n)

		tb.runCleanups()
	})
	t.Run("takes any testing.TB, not only a testing.T", func(t *testing.T) {
		// The signature says testing.TB; a *testing.B has to work too. It is
		// also the only TB here that is neither a *testing.T nor our fake, so
		// it is the one thing that proves New asks for nothing a T alone has.
		var (
			names  []string
			bodies []string
			rm     metricdata.ResourceMetrics
			n      int64
		)

		result := testing.Benchmark(func(b *testing.B) {
			h := otxtest.New(b)
			ctx := h.Into(b.Context())

			h.Reset()
			for b.Loop() {
				recordHit(ctx, 1)
			}

			emit(ctx, "op")

			names = namesOf(h.Ended())
			bodies = bodiesOf(h.Records())
			rm = h.Collect(ctx)
			n = int64(b.N) + 1
		})

		require.NotZero(t, result.N, "a benchmark that failed reports a zero result")
		require.Equal(t, []string{"op"}, names)
		require.Equal(t, []string{"op"}, bodies)
		require.Equal(t, n, sumOf(t, rm, "hits"))
	})
}

func TestHarnessFields(t *testing.T) {
	// New is the documented way to get a Harness, but the fields are exported
	// so that a test can wire the recorders itself. The methods that do not
	// need the testing.TB work on such a value; Collect does need it, which is
	// why New exists.
	t.Run("a hand-built harness serves the methods that do not need the tb", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		logs := &otxmem.LogExporter{}

		h := &otxtest.Harness{
			Spans: spans,
			Logs:  logs,
			Otx: otx.New(
				otx.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))),
				otx.WithLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))),
			),
		}
		t.Cleanup(func() { require.NoError(t, h.Otx.Shutdown(context.Background())) })

		ctx := h.Into(t.Context())
		_, span := otx.TraceStart(ctx, "op")
		span.End()
		log.From(ctx).Info("hello")

		require.Equal(t, []string{"op"}, namesOf(h.Ended()))
		require.Equal(t, []string{"hello"}, bodiesOf(h.Records()))

		h.Reset()
		require.Empty(t, h.Ended())
		require.Empty(t, h.Records())
	})
}

func TestInto(t *testing.T) {
	t.Run("puts the otx in the context", func(t *testing.T) {
		h := otxtest.New(t)

		ctx := h.Into(t.Context())
		v, ok := otx.FromOK(ctx)
		require.True(t, ok)
		require.Same(t, h.Otx, v)

		_, ok = otx.FromOK(t.Context())
		require.False(t, ok, "a plain context carries none")
	})
	t.Run("does not modify the given context", func(t *testing.T) {
		h := otxtest.New(t)

		ctx := t.Context()
		_ = h.Into(ctx)

		_, ok := otx.FromOK(ctx)
		require.False(t, ok)
	})
	t.Run("the slog logger of the context reaches the exporter", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		log.From(ctx).Info("hello", "answer", 42)

		records := h.Records()
		require.Len(t, records, 1)
		require.Equal(t, "hello", records[0].Body().AsString())
		require.Equal(t, otellog.SeverityInfo, records[0].Severity())
	})
	t.Run("records are correlated with the span of the context", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		ctx, span := otx.TraceStart(ctx, "op")
		log.From(ctx).Info("hello")
		span.End()

		require.Len(t, h.Ended(), 1)
		require.Len(t, h.Records(), 1)
		require.Equal(t, h.Ended()[0].SpanContext().TraceID(), h.Records()[0].TraceID())
		require.Equal(t, h.Ended()[0].SpanContext().SpanID(), h.Records()[0].SpanID())
	})
}

func TestEnded(t *testing.T) {
	t.Run("returns the ended spans in order", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		require.Empty(t, h.Ended())

		for _, name := range []string{"foo", "bar", "baz"} {
			_, span := otx.TraceStart(ctx, name)
			span.End()
		}

		require.Equal(t, []string{"foo", "bar", "baz"}, namesOf(h.Ended()))
	})
	t.Run("does not return a span that has not ended", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		_, span := otx.TraceStart(ctx, "op")
		require.Empty(t, h.Ended())

		span.End()
		require.Equal(t, []string{"op"}, namesOf(h.Ended()))
	})
	t.Run("returns a copy of the slice", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		_, span := otx.TraceStart(ctx, "foo")
		span.End()

		spans := h.Ended()
		require.Len(t, spans, 1)
		spans[0] = nil
		spans = append(spans, nil)
		require.Len(t, spans, 2)

		require.Equal(t, []string{"foo"}, namesOf(h.Ended()))
	})
}

func TestRecords(t *testing.T) {
	t.Run("returns the exported records in order", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		require.Empty(t, h.Records())

		for _, msg := range []string{"foo", "bar", "baz"} {
			log.From(ctx).Info(msg)
		}

		require.Equal(t, []string{"foo", "bar", "baz"}, bodiesOf(h.Records()))
	})
	t.Run("returns a copy of the slice", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		log.From(ctx).Info("foo")
		log.From(ctx).Info("bar")

		records := h.Records()
		require.Len(t, records, 2)

		records[0] = sdklog.Record{}
		records[1].SetBody(otellog.StringValue("mutated"))
		records = append(records, sdklog.Record{})
		require.Len(t, records, 3)

		require.Equal(t, []string{"foo", "bar"}, bodiesOf(h.Records()))
	})
}

func TestCollect(t *testing.T) {
	t.Run("returns the metrics recorded so far", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		require.Empty(t, h.Collect(ctx).ScopeMetrics, "nothing was recorded yet")

		recordHit(ctx, 1)
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))

		recordHit(ctx, 2)
		require.Equal(t, int64(3), sumOf(t, h.Collect(ctx), "hits"))

		rm := h.Collect(ctx)
		require.Len(t, rm.ScopeMetrics, 1)
		require.Equal(t, otx.Scope, rm.ScopeMetrics[0].Scope.Name)
		require.Equal(t, int64(3), sumOf(t, rm, "hits"), "collecting again does not consume them")
	})
	t.Run("honours a cancelled context", func(t *testing.T) {
		tb := &fakeTB{}
		h := otxtest.New(tb)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.True(t, recoverFatal(t, func() { h.Collect(ctx) }))
		require.Len(t, tb.fatals, 1)
		require.Contains(t, tb.fatals[0], "otxtest: collect metrics")
		require.Contains(t, tb.fatals[0], context.Canceled.Error())

		tb.runCleanups()
		require.Empty(t, tb.errors)
	})
	t.Run("fails the test when the reader is shut down", func(t *testing.T) {
		tb := &fakeTB{}
		h := otxtest.New(tb)
		ctx := h.Into(t.Context())

		recordHit(ctx, 1)
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))

		require.NoError(t, h.Reader.Shutdown(ctx))

		require.True(t,
			recoverFatal(t, func() { h.Collect(ctx) }),
			"a collect that cannot be served must stop the test rather than return a zero value",
		)
		require.Len(t, tb.fatals, 1)
		require.Contains(t, tb.fatals[0], "otxtest: collect metrics")
		require.Contains(t, tb.fatals[0], sdkmetric.ErrReaderShutdown.Error())
		require.Empty(t, tb.errors)
	})
	t.Run("marks itself as a test helper", func(t *testing.T) {
		// A Fatalf that is not preceded by Helper points at the line inside
		// Collect, which tells the reader nothing about which collect failed.
		tb := &fakeTB{}
		h := otxtest.New(tb)

		_, _, before := tb.snapshot()
		h.Collect(t.Context())

		_, _, after := tb.snapshot()
		require.Equal(t, before+1, after)

		tb.runCleanups()
	})
	t.Run("reports through the tb even off the test goroutine", func(t *testing.T) {
		// This documents what actually happens rather than what one would
		// want. Collect fails through testing.TB.Fatalf, which the testing
		// package requires be called from the goroutine running the test: from
		// anywhere else the real Fatalf marks the test failed and then ends
		// the *calling* goroutine, so the test carries on to its own
		// assertions instead of stopping. The fake stands in for that here.
		//
		// The upshot for a caller: collect on the test goroutine, or check the
		// reader yourself.
		tb := &fakeTB{}
		h := otxtest.New(tb)
		ctx := h.Into(t.Context())

		recordHit(ctx, 1)
		require.NoError(t, h.Reader.Shutdown(ctx))

		done := make(chan bool, 1)
		go func() {
			done <- recoverFatal(t, func() { h.Collect(ctx) })
		}()

		require.True(t, <-done, "the failure is reported from the goroutine that called Collect")

		errs, fatals, _ := tb.snapshot()
		require.Len(t, fatals, 1)
		require.Contains(t, fatals[0], sdkmetric.ErrReaderShutdown.Error())
		require.Empty(t, errs)
	})
}

func TestReset(t *testing.T) {
	t.Run("discards the spans and the records and leaves the harness usable", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		emit(ctx, "before")

		spans := h.Ended()
		records := h.Records()

		h.Reset()
		require.Empty(t, h.Ended())
		require.Empty(t, h.Records())
		require.Equal(t, []string{"before"}, namesOf(spans), "already returned spans are untouched")
		require.Equal(t, []string{"before"}, bodiesOf(records), "already returned records are untouched")

		emit(ctx, "after")
		require.Equal(t, []string{"after"}, namesOf(h.Ended()))
		require.Equal(t, []string{"after"}, bodiesOf(h.Records()))
	})
	t.Run("leaves the metrics alone because they are pulled", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		recordHit(ctx, 1)
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))

		h.Reset()
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))

		recordHit(ctx, 1)
		require.Equal(t, int64(2), sumOf(t, h.Collect(ctx), "hits"))
	})
	t.Run("is a no-op on a fresh harness", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		h.Reset()
		h.Reset()

		emit(ctx, "op")
		require.Len(t, h.Ended(), 1)
		require.Len(t, h.Records(), 1)
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))
	})
}

func TestCleanup(t *testing.T) {
	t.Run("registers exactly one cleanup", func(t *testing.T) {
		tb := &fakeTB{}
		otxtest.New(tb)

		require.Len(t, tb.cleanups, 1)
		require.Empty(t, tb.errors)
		require.Empty(t, tb.fatals)

		tb.runCleanups()
	})
	t.Run("shuts the otx down", func(t *testing.T) {
		tb := &fakeTB{}
		h := otxtest.New(tb)
		ctx := h.Into(t.Context())

		emit(ctx, "before")
		require.Len(t, h.Ended(), 1)
		require.Len(t, h.Records(), 1)
		require.Equal(t, int64(1), sumOf(t, h.Collect(ctx), "hits"))

		tb.runCleanups()
		require.Empty(t, tb.errors, "a clean shutdown is silent")
		require.Empty(t, tb.fatals)

		_, span := otx.TraceStart(ctx, "after")
		span.End()
		log.From(ctx).Info("after")

		require.Equal(t, []string{"before"}, namesOf(h.Ended()), "the tracer provider is shut down")
		require.Equal(t, []string{"before"}, bodiesOf(h.Records()), "the logger provider is shut down")
		require.True(t,
			recoverFatal(t, func() { h.Collect(ctx) }),
			"the meter provider is shut down, which takes the reader with it",
		)
	})
	t.Run("reports a failed shutdown through the test", func(t *testing.T) {
		tb := &fakeTB{}
		provider := &failingProvider{err: errors.New("boom")}
		h := otxtest.New(tb, otx.WithTracerProvider(provider))

		require.Len(t, tb.cleanups, 1)
		tb.runCleanups()

		require.Equal(t, 1, provider.shutdown_n)
		require.Len(t, tb.errors, 1)
		require.Contains(t, tb.errors[0], "otxtest", "the message names the package that failed the test")
		require.Contains(t, tb.errors[0], "shutdown")
		require.Contains(t, tb.errors[0], "boom")
		require.Empty(t, tb.fatals, "a failed shutdown does not stop the test")

		// The tracer provider the option displaced is not owned by the Otx, so
		// nothing shut it down.
		require.NoError(t, h.TracerProvider.Shutdown(t.Context()))
	})
	t.Run("reports a failed shutdown of the logger provider as well", func(t *testing.T) {
		// The error of any owned provider reaches the same report, which is
		// what makes the joined error of Otx.Shutdown worth passing on.
		tb := &fakeTB{}
		provider := &failingProvider{err: errors.New("kaboom")}
		h := otxtest.New(tb, otx.WithLoggerProvider(provider))

		tb.runCleanups()

		require.Equal(t, 1, provider.shutdown_n)
		require.Len(t, tb.errors, 1)
		require.Contains(t, tb.errors[0], "otxtest")
		require.Contains(t, tb.errors[0], "kaboom")

		require.NoError(t, h.LoggerProvider.Shutdown(t.Context()))
	})
	t.Run("a shutdown the test did itself is not repeated but is still reported", func(t *testing.T) {
		// Otx.Shutdown is once-guarded: the provider is shut down once, and
		// the cleanup gets the remembered error back. So a test that shuts the
		// harness down itself and handles the error still has it reported.
		tb := &fakeTB{}
		provider := &failingProvider{err: errors.New("boom")}
		h := otxtest.New(tb, otx.WithTracerProvider(provider))

		require.ErrorContains(t, h.Otx.Shutdown(t.Context()), "boom")
		tb.runCleanups()

		require.Equal(t, 1, provider.shutdown_n, "the provider is shut down once")
		require.Len(t, tb.errors, 1)
		require.Contains(t, tb.errors[0], "boom")

		require.NoError(t, h.TracerProvider.Shutdown(t.Context()))
	})
}

func TestConcurrentUse(t *testing.T) {
	const n = 16

	t.Run("emitting every signal from many goroutines is safe", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		start := make(chan struct{})
		done := make(chan struct{})

		wg_write := sync.WaitGroup{}
		for i := range n {
			wg_write.Add(1)
			go func() {
				defer wg_write.Done()
				<-start
				emit(ctx, fmt.Sprintf("op-%d", i))
			}()
		}

		// The readers run while the writers do, so that a reader that handed
		// out the storage it keeps writing into would be caught by -race.
		wg_read := sync.WaitGroup{}
		for range n {
			wg_read.Add(1)
			go func() {
				defer wg_read.Done()
				<-start
				for {
					select {
					case <-done:
						return
					default:
					}

					_ = namesOf(h.Ended())
					_ = bodiesOf(h.Records())
					_ = h.Collect(ctx)
				}
			}()
		}

		close(start)
		wg_write.Wait()
		close(done)
		wg_read.Wait()

		require.Len(t, h.Ended(), n)
		require.Len(t, h.Records(), n)
		require.Equal(t, int64(n), sumOf(t, h.Collect(ctx), "hits"))

		names := namesOf(h.Ended())
		bodies := bodiesOf(h.Records())
		for i := range n {
			require.Contains(t, names, fmt.Sprintf("op-%d", i))
			require.Contains(t, bodies, fmt.Sprintf("op-%d", i))
		}
	})
	t.Run("resetting while emitting is safe", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		start := make(chan struct{})

		wg := sync.WaitGroup{}
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				emit(ctx, fmt.Sprintf("op-%d", i))
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				h.Reset()
			}()
		}

		close(start)
		wg.Wait()

		// How much survives depends on the interleaving; the metrics do not,
		// since Reset does not touch them.
		require.LessOrEqual(t, len(h.Ended()), n)
		require.LessOrEqual(t, len(h.Records()), n)
		require.Equal(t, int64(n), sumOf(t, h.Collect(ctx), "hits"))
	})
}

// recordHit adds n to a counter taken from the meter the Otx carries. The
// instrument constructors of the otx package would do the same in one line,
// but the harness is what is under test here, so it goes through the plain
// metric API.
func recordHit(ctx context.Context, n int64) {
	// The error is ignored rather than asserted: the SDK hands back a usable
	// instrument alongside it, and the name here is a constant.
	counter, _ := otx.Meter(ctx).Int64Counter("hits")
	counter.Add(ctx, n)
}
