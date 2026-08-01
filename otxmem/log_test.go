package otxmem_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// newRecord builds an SDK record as an exporter would receive one.
func newRecord(body string, attrs ...log.KeyValue) sdklog.Record {
	r := sdklog.Record{}
	r.SetBody(log.StringValue(body))
	r.SetSeverity(log.SeverityInfo)
	r.AddAttributes(attrs...)

	return r
}

func bodiesOf(records []sdklog.Record) []string {
	rst := make([]string, len(records))
	for i := range records {
		rst[i] = records[i].Body().AsString()
	}

	return rst
}

func attrOf(t *testing.T, r sdklog.Record, key string) log.Value {
	t.Helper()

	var (
		rst      log.Value
		is_found bool
	)
	r.WalkAttributes(func(kv log.KeyValue) bool {
		if kv.Key != key {
			return true
		}

		rst = kv.Value
		is_found = true

		return false
	})
	require.True(t, is_found, "attribute %q not found", key)

	return rst
}

func TestLogExporter(t *testing.T) {
	t.Run("collects what a logger provider exports, in order", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())

		logger := provider.Logger("otxmem-test")
		for _, msg := range []string{"foo", "bar", "baz"} {
			r := log.Record{}
			r.SetBody(log.StringValue(msg))
			r.SetSeverity(log.SeverityWarn)
			r.AddAttributes(log.String("msg", msg))
			logger.Emit(t.Context(), r)
		}

		require.Equal(t, 3, exporter.Len())

		records := exporter.Records()
		require.Len(t, records, 3)
		require.Equal(t, []string{"foo", "bar", "baz"}, bodiesOf(records))
		for i, msg := range []string{"foo", "bar", "baz"} {
			require.Equal(t, log.SeverityWarn, records[i].Severity())
			require.Equal(t, msg, attrOf(t, records[i], "msg").AsString())
			require.Equal(t, "otxmem-test", records[i].InstrumentationScope().Name)
		}
	})

	t.Run("collects records of a single export call in order", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{
			newRecord("foo"),
			newRecord("bar"),
		}))
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{
			newRecord("baz"),
		}))
		require.NoError(t, exporter.Export(t.Context(), nil))

		require.Equal(t, 3, exporter.Len())
		require.Equal(t, []string{"foo", "bar", "baz"}, bodiesOf(exporter.Records()))
	})

	t.Run("records returns a copy of the slice", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{
			newRecord("foo"),
			newRecord("bar"),
		}))

		records := exporter.Records()
		require.Len(t, records, 2)

		records[0] = newRecord("mutated")
		records[1] = sdklog.Record{}
		records = append(records, newRecord("appended"))
		require.Len(t, records, 3)

		require.Equal(t, 2, exporter.Len())
		require.Equal(t, []string{"foo", "bar"}, bodiesOf(exporter.Records()))
	})

	t.Run("records are cloned, not retained", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}

		// More attributes than a record holds inline, so that some of them
		// spill into the slice the record allocates; that slice is the part
		// a shallow copy would share with the caller.
		attrs := make([]log.KeyValue, 8)
		for i := range attrs {
			attrs[i] = log.Int(fmt.Sprintf("k%d", i), i)
		}

		records := []sdklog.Record{newRecord("origin", attrs...)}
		require.NoError(t, exporter.Export(t.Context(), records))

		// The SDK reuses the storage behind a record once Export returns.
		records[0].SetBody(log.StringValue("mutated"))
		records[0].SetSeverity(log.SeverityError)
		records[0].AddAttributes(log.Int("k0", 100), log.Int("k7", 700))
		records[0] = sdklog.Record{}

		stored := exporter.Records()
		require.Len(t, stored, 1)
		require.Equal(t, "origin", stored[0].Body().AsString())
		require.Equal(t, log.SeverityInfo, stored[0].Severity())
		require.Equal(t, int64(0), attrOf(t, stored[0], "k0").AsInt64())
		require.Equal(t, int64(7), attrOf(t, stored[0], "k7").AsInt64())
	})

	t.Run("spilled attributes of returned records are shared with the exporter", func(t *testing.T) {
		// Documents the current behaviour: Records copies the slice and the
		// record structs, but the attributes that did not fit inline still
		// live in storage shared with the exporter, so writing one back
		// through a returned record is visible to the exporter.
		exporter := &otxmem.LogExporter{}

		attrs := make([]log.KeyValue, 8)
		for i := range attrs {
			attrs[i] = log.Int(fmt.Sprintf("k%d", i), i)
		}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("origin", attrs...)}))

		out := exporter.Records()
		require.Len(t, out, 1)
		out[0].SetBody(log.StringValue("mutated"))
		out[0].AddAttributes(log.Int("k0", 100), log.Int("k7", 700))

		stored := exporter.Records()
		require.Len(t, stored, 1)
		require.Equal(t, "origin", stored[0].Body().AsString(), "the record struct itself is copied")
		require.Equal(t, int64(0), attrOf(t, stored[0], "k0").AsInt64(), "inline attributes are copied")
		require.Equal(t, int64(700), attrOf(t, stored[0], "k7").AsInt64(), "spilled attributes are not")
	})

	t.Run("export honours a cancelled context", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("kept")}))

		ctx_cancelled, cancel := context.WithCancel(t.Context())
		cancel()

		err := exporter.Export(ctx_cancelled, []sdklog.Record{newRecord("dropped")})
		require.ErrorIs(t, err, context.Canceled)

		ctx_expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
		defer cancel()

		err = exporter.Export(ctx_expired, []sdklog.Record{newRecord("dropped")})
		require.ErrorIs(t, err, context.DeadlineExceeded)

		require.Equal(t, 1, exporter.Len())
		require.Equal(t, []string{"kept"}, bodiesOf(exporter.Records()))
	})

	t.Run("export is a no-op after shutdown", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("before")}))

		require.NoError(t, exporter.Shutdown(t.Context()))
		require.NoError(t, exporter.Shutdown(t.Context()), "shutdown is safe to call twice")

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("after")}))
		require.NoError(t, exporter.ForceFlush(t.Context()))

		// The context is still inspected first, so a shut down exporter
		// reports the context error rather than the nil of a no-op.
		ctx_cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t,
			exporter.Export(ctx_cancelled, []sdklog.Record{newRecord("after")}),
			context.Canceled,
		)

		require.Equal(t, 1, exporter.Len(), "records taken before the shutdown are still readable")
		require.Equal(t, []string{"before"}, bodiesOf(exporter.Records()))
	})

	t.Run("shutdown through a logger provider stops the recording", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
		)
		logger := provider.Logger("otxmem-test")

		r := log.Record{}
		r.SetBody(log.StringValue("before"))
		logger.Emit(t.Context(), r)

		require.NoError(t, provider.ForceFlush(t.Context()))
		require.NoError(t, provider.Shutdown(t.Context()))

		r.SetBody(log.StringValue("after"))
		logger.Emit(t.Context(), r)

		require.Equal(t, []string{"before"}, bodiesOf(exporter.Records()))
	})

	t.Run("force flush is a no-op", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.ForceFlush(t.Context()))
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("foo")}))
		require.NoError(t, exporter.ForceFlush(t.Context()))

		require.Equal(t, []string{"foo"}, bodiesOf(exporter.Records()))
	})

	t.Run("reset discards the records and leaves the exporter usable", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("foo")}))

		records := exporter.Records()
		exporter.Reset()

		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())
		require.Equal(t, []string{"foo"}, bodiesOf(records), "already returned records are untouched")

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("bar")}))
		require.Equal(t, []string{"bar"}, bodiesOf(exporter.Records()))
	})

	t.Run("the zero value is ready to use", func(t *testing.T) {
		var exporter otxmem.LogExporter

		require.Zero(t, exporter.Len())
		require.Empty(t, exporter.Records())
		require.NoError(t, exporter.ForceFlush(t.Context()))
		exporter.Reset()

		require.NoError(t, exporter.Export(t.Context(), []sdklog.Record{newRecord("foo")}))
		require.Equal(t, 1, exporter.Len())
		require.Equal(t, []string{"foo"}, bodiesOf(exporter.Records()))
	})
}

func TestLogExporterConcurrency(t *testing.T) {
	const (
		num_workers = 8
		num_rounds  = 64
		num_records = 4
	)

	t.Run("exports and reads from many goroutines are safe", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(
			exporter,
			// The batch processor exports from its own goroutine; a short
			// interval and a small batch make it do so while the writers and
			// the readers below are still running.
			sdklog.WithExportInterval(time.Millisecond),
			sdklog.WithExportMaxBatchSize(8),
			sdklog.WithMaxQueueSize(num_workers*num_rounds*4),
		)))
		logger := provider.Logger("otxmem-test")

		ctx := t.Context()

		start := make(chan struct{})
		done := make(chan struct{})

		wg_write := sync.WaitGroup{}
		for w := range num_workers {
			wg_write.Add(1)
			go func() {
				defer wg_write.Done()
				<-start
				for i := range num_rounds {
					r := log.Record{}
					r.SetBody(log.StringValue(fmt.Sprintf("emitted-%d-%d", w, i)))
					logger.Emit(ctx, r)
				}
			}()

			wg_write.Add(1)
			go func() {
				defer wg_write.Done()
				<-start
				for i := range num_rounds {
					records := make([]sdklog.Record, num_records)
					for j := range records {
						records[j] = newRecord(fmt.Sprintf("exported-%d-%d-%d", w, i, j))
					}
					if err := exporter.Export(ctx, records); err != nil {
						t.Errorf("export: %v", err)
						return
					}
				}
			}()
		}

		wg_read := sync.WaitGroup{}
		for range num_workers {
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

					records := exporter.Records()
					if l := exporter.Len(); l < len(records) {
						t.Errorf("len %d is behind the %d records it returned", l, len(records))
						return
					}
					for i := range records {
						_ = records[i].Body().AsString()
					}
				}
			}()
		}

		close(start)
		wg_write.Wait()
		close(done)
		wg_read.Wait()

		require.NoError(t, provider.ForceFlush(ctx))

		num_emitted := num_workers * num_rounds
		num_exported := num_workers * num_rounds * num_records
		require.Equal(t, num_emitted+num_exported, exporter.Len())
		require.Len(t, exporter.Records(), num_emitted+num_exported)

		require.NoError(t, provider.Shutdown(ctx))
	})

	t.Run("shutdown races with exports without losing what was recorded", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}

		ctx := t.Context()
		require.NoError(t, exporter.Export(ctx, []sdklog.Record{newRecord("first")}))

		start := make(chan struct{})

		wg := sync.WaitGroup{}
		for w := range num_workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
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

			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_ = exporter.Records()
				_ = exporter.Len()
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := exporter.Shutdown(ctx); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}()

		close(start)
		wg.Wait()

		records := exporter.Records()
		require.NotEmpty(t, records)
		require.Equal(t, "first", records[0].Body().AsString())
		require.LessOrEqual(t, len(records), 1+num_workers*num_rounds)
		require.Equal(t, len(records), exporter.Len())

		// Recording has stopped for good.
		require.NoError(t, exporter.Export(ctx, []sdklog.Record{newRecord("after")}))
		require.Equal(t, len(records), exporter.Len())
	})
}
