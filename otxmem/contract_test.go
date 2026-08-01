package otxmem_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

// TestLogExporterContract pins the clauses of the [sdklog.Exporter] contract
// the exporter keeps, and - just as deliberately - the ones it does not, so
// that a change either way is noticed.
func TestLogExporterContract(t *testing.T) {
	t.Run("shutdown ignores a cancelled context", func(t *testing.T) {
		// The [sdklog.Exporter] contract asks Shutdown to honour the deadline
		// or cancellation of its context and to report it. This one never
		// reads the context: it reports success and still latches closed.
		// Documented as it behaves, not as the interface asks.
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("before")}))

		ctx_cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		require.NoError(t, exporter.Shutdown(ctx_cancelled))

		ctx_expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
		defer cancel()
		require.NoError(t, exporter.Shutdown(ctx_expired))

		// The first shutdown took effect in spite of the dead context.
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("after")}))
		require.Equal(t, []string{"before"}, bodiesOf(exporter.Records()))
	})

	t.Run("force flush ignores a cancelled context", func(t *testing.T) {
		// Same deviation as Shutdown, and harmless for the same reason: there
		// is nothing buffered that a deadline could cut short.
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("foo")}))

		ctx_cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		require.NoError(t, exporter.ForceFlush(ctx_cancelled))

		ctx_expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
		defer cancel()
		require.NoError(t, exporter.ForceFlush(ctx_expired))

		require.Equal(t, []string{"foo"}, bodiesOf(exporter.Records()), "nothing was flushed away")
	})

	t.Run("reset does not reopen a shut down exporter", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("before")}))
		require.NoError(t, exporter.Shutdown(t.Context()))

		exporter.Reset()
		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("after")}))
		require.Zero(t, exporter.Len(), "reset clears the records but does not unlatch the shutdown")
		require.Empty(t, exporter.Records())
	})

	t.Run("export does not retain the records slice", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}

		records := []sdklog.Record{newRecord("foo"), newRecord("bar")}
		require.NoError(t, exporter.Export(t.Context(), records))

		// The SDK owns the slice again the moment Export returns and is free
		// to refill it for the next batch.
		clear(records)
		records[0] = newRecord("reused-0")
		records[1] = newRecord("reused-1")

		require.Equal(t, []string{"foo", "bar"}, bodiesOf(exporter.Records()))
		require.Equal(t, 2, exporter.Len())
	})

	t.Run("every field of an exported record is preserved", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}

		trace_id, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
		require.NoError(t, err)
		span_id, err := trace.SpanIDFromHex("1112131415161718")
		require.NoError(t, err)

		ts := time.Date(2026, time.July, 31, 12, 34, 56, 789, time.UTC)
		observed := ts.Add(time.Second)

		r := sdklog.Record{}
		r.SetBody(log.StringValue("body"))
		r.SetEventName("event.name")
		r.SetSeverity(log.SeverityError)
		r.SetSeverityText("ERROR")
		r.SetTimestamp(ts)
		r.SetObservedTimestamp(observed)
		r.SetTraceID(trace_id)
		r.SetSpanID(span_id)
		r.SetTraceFlags(trace.FlagsSampled)
		// More than the five attributes a record holds inline, so that the
		// spilled ones are exercised too.
		for i := range 8 {
			r.AddAttributes(log.Int(fmt.Sprintf("k%d", i), i))
		}

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{r}))

		stored := exporter.Records()
		require.Len(t, stored, 1)

		got := stored[0]
		require.Equal(t, "body", got.Body().AsString())
		require.Equal(t, "event.name", got.EventName())
		require.Equal(t, log.SeverityError, got.Severity())
		require.Equal(t, "ERROR", got.SeverityText())
		require.True(t, ts.Equal(got.Timestamp()), "timestamp %s", got.Timestamp())
		require.True(t, observed.Equal(got.ObservedTimestamp()), "observed timestamp %s", got.ObservedTimestamp())
		require.Equal(t, trace_id, got.TraceID())
		require.Equal(t, span_id, got.SpanID())
		require.Equal(t, trace.FlagsSampled, got.TraceFlags())
		require.Equal(t, 8, got.AttributesLen())
		require.Zero(t, got.DroppedAttributes())
		for i := range 8 {
			require.Equal(t, int64(i), attrOf(t, got, fmt.Sprintf("k%d", i)).AsInt64())
		}
	})

	t.Run("the resource and scope a provider attaches reach the records", func(t *testing.T) {
		res := resource.NewSchemaless(attribute.String("service.name", "otxmem-test"))

		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

		r := log.Record{}
		r.SetBody(log.StringValue("foo"))
		provider.Logger("scope-name", log.WithInstrumentationVersion("v1.2.3")).Emit(t.Context(), r)

		records := exporter.Records()
		require.Len(t, records, 1)
		require.Equal(t, res, records[0].Resource())
		require.Equal(t, "scope-name", records[0].InstrumentationScope().Name)
		require.Equal(t, "v1.2.3", records[0].InstrumentationScope().Version)
	})

	t.Run("the dropped attribute count of a record is preserved", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(
			sdklog.WithAttributeCountLimit(2),
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

		r := log.Record{}
		r.SetBody(log.StringValue("foo"))
		r.AddAttributes(log.Int("a", 1), log.Int("b", 2), log.Int("c", 3), log.Int("d", 4))
		provider.Logger("otxmem-test").Emit(t.Context(), r)

		records := exporter.Records()
		require.Len(t, records, 1)
		require.Equal(t, 2, records[0].AttributesLen())
		require.Equal(t, 2, records[0].DroppedAttributes())
	})

	t.Run("records survive a batch processor that exports from its own goroutine", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(
			exporter,
			// Long enough that nothing is exported until ForceFlush asks; the
			// point is that the export happens on the processor goroutine and
			// the records are still readable here afterwards.
			sdklog.WithExportInterval(time.Hour),
		)))

		for _, msg := range []string{"foo", "bar", "baz"} {
			r := log.Record{}
			r.SetBody(log.StringValue(msg))
			r.AddAttributes(log.String("msg", msg))
			provider.Logger("otxmem-test").Emit(t.Context(), r)
		}

		require.NoError(t, provider.ForceFlush(t.Context()))
		require.Equal(t, []string{"foo", "bar", "baz"}, bodiesOf(exporter.Records()))

		require.NoError(t, provider.Shutdown(t.Context()))
	})
}

func TestLogExporterResetConcurrency(t *testing.T) {
	const (
		num_workers = 8
		num_rounds  = 64
	)

	t.Run("resets interleave with exports and reads without corrupting a snapshot", func(t *testing.T) {
		// A Reset makes the record count nondeterministic, so the assertion
		// is on what stays true regardless of the interleaving: a snapshot
		// only ever holds bodies that were really exported, never the same
		// one twice - the records only grow by appending between resets - and
		// the exporter is empty and usable once everything is quiescent.
		exporter := &otxmem.LogExporter{}
		ctx := t.Context()

		is_exported := map[string]struct{}{}
		for w := range num_workers {
			for i := range num_rounds {
				is_exported[fmt.Sprintf("exported-%d-%d", w, i)] = struct{}{}
			}
		}

		start := make(chan struct{})
		done := make(chan struct{})

		wg_write := sync.WaitGroup{}
		for w := range num_workers {
			wg_write.Add(1)
			go func() {
				defer wg_write.Done()
				<-start
				for i := range num_rounds {
					if err := exporter.Export(ctx, []sdklog.Record{
						newRecord(fmt.Sprintf("exported-%d-%d", w, i)),
					}); err != nil {
						t.Errorf("export: %v", err)
						return
					}
				}
			}()
		}

		wg_bg := sync.WaitGroup{}
		wg_bg.Add(1)
		go func() {
			defer wg_bg.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}

				exporter.Reset()
			}
		}()

		for range num_workers {
			wg_bg.Add(1)
			go func() {
				defer wg_bg.Done()
				<-start
				for {
					select {
					case <-done:
						return
					default:
					}

					records := exporter.Records()
					seen := make(map[string]struct{}, len(records))
					for i := range records {
						body := records[i].Body().AsString()
						if _, ok := is_exported[body]; !ok {
							t.Errorf("snapshot holds a body that was never exported: %q", body)
							return
						}
						if _, ok := seen[body]; ok {
							t.Errorf("snapshot holds %q twice", body)
							return
						}
						seen[body] = struct{}{}
					}
					_ = exporter.Len()
				}
			}()
		}

		close(start)
		wg_write.Wait()
		close(done)
		wg_bg.Wait()

		// Quiescent again, so the exporter is back to a determinate state.
		exporter.Reset()
		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())

		require.NoError(t, exporter.Export(ctx, []sdklog.Record{newRecord("after")}))
		require.Equal(t, 1, exporter.Len())
		require.Equal(t, []string{"after"}, bodiesOf(exporter.Records()))
	})

	t.Run("shutdown is safe concurrently with itself and with the other methods", func(t *testing.T) {
		// The exporter contract allows Shutdown to be called concurrently
		// with itself and with every other method.
		exporter := &otxmem.LogExporter{}
		ctx := t.Context()

		start := make(chan struct{})

		wg := sync.WaitGroup{}
		run := func(f func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				f()
			}()
		}

		for w := range num_workers {
			run(func() {
				if err := exporter.Shutdown(ctx); err != nil {
					t.Errorf("shutdown: %v", err)
				}
			})
			run(func() {
				if err := exporter.ForceFlush(ctx); err != nil {
					t.Errorf("force flush: %v", err)
				}
			})
			run(func() {
				for i := range num_rounds {
					if err := exporter.Export(ctx, []sdklog.Record{
						newRecord(fmt.Sprintf("exported-%d-%d", w, i)),
					}); err != nil {
						t.Errorf("export: %v", err)
						return
					}
				}
			})
			run(func() { _ = exporter.Records() })
			run(func() { _ = exporter.Len() })
			run(exporter.Reset)
		}

		close(start)
		wg.Wait()

		// Whatever the interleaving was, the exporter is closed for good.
		exporter.Reset()
		require.NoError(t, exporter.Export(ctx, []sdklog.Record{newRecord("after")}))
		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())
		require.NoError(t, exporter.ForceFlush(ctx))
		require.NoError(t, exporter.Shutdown(ctx))
	})
}
