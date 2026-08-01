// Package otxmem holds in-memory OpenTelemetry exporters for use in tests.
//
// Only logs are covered, because upstream already provides the other two:
// [go.opentelemetry.io/otel/sdk/trace/tracetest.NewInMemoryExporter] for spans
// and [go.opentelemetry.io/otel/sdk/metric.NewManualReader] for metrics.
package otxmem

import (
	"context"
	"sync"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LogExporter collects exported log records in memory.
//
// It is safe for concurrent use, which matters because
// [go.opentelemetry.io/otel/sdk/log.BatchProcessor] exports from its own
// goroutine while the test reads the result. The zero value is ready to use:
//
//	exporter := &otxmem.LogExporter{}
//	provider := sdklog.NewLoggerProvider(
//		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
//	)
type LogExporter struct {
	mu       sync.Mutex
	records  []sdklog.Record
	is_close bool
}

var _ sdklog.Exporter = (*LogExporter)(nil)

// Export records a copy of each record. It is a no-op after
// [LogExporter.Shutdown].
func (h *LogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.is_close {
		return nil
	}

	// The SDK reuses the storage behind a Record once Export returns, so they
	// are cloned rather than retained.
	for i := range records {
		h.records = append(h.records, records[i].Clone())
	}

	return nil
}

// Shutdown stops recording. Subsequent calls to [LogExporter.Export] are
// no-ops, as the exporter contract requires. Records already taken remain
// readable.
func (h *LogExporter) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.is_close = true

	return nil
}

// ForceFlush is a no-op; records are stored as they arrive.
func (h *LogExporter) ForceFlush(ctx context.Context) error {
	return nil
}

// Records returns the records exported so far, in order.
func (h *LogExporter) Records() []sdklog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]sdklog.Record(nil), h.records...)
}

// Len reports how many records have been exported.
func (h *LogExporter) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.records)
}

// Reset discards the recorded records, so that one exporter can serve several
// phases of a test. It does not reopen an exporter that was shut down: the
// exporter contract is that shutdown is final, and resurrecting it would make
// the result of a Reset racing a Shutdown undefined.
func (h *LogExporter) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
}
