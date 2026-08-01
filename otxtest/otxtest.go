// Package otxtest wires an [otx.Otx] onto in-memory recorders for all three
// signals, so that a test can assert on the spans, log records and metrics an
// operation produced without repeating the SDK setup.
//
//	func TestThing(t *testing.T) {
//		h := otxtest.New(t)
//		ctx := h.Into(t.Context())
//
//		doTheThing(ctx)
//
//		require.Len(t, h.Ended(), 1)
//		require.Equal(t, "hello", h.Records()[0].Body().AsString())
//	}
//
// Everything is synchronous: spans and log records are recorded as they are
// produced, and metrics are pulled on demand, so there is nothing to wait for.
package otxtest

import (
	"context"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Harness is an [otx.Otx] and the recorders behind it.
//
// The fields are exported for the cases the methods do not cover; reach for
// the methods first.
type Harness struct {
	// Otx is wired to the three recorders below and is shut down when the test
	// ends.
	Otx *otx.Otx

	Spans  *tracetest.SpanRecorder
	Logs   *otxmem.LogExporter
	Reader *sdkmetric.ManualReader

	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider

	tb testing.TB
}

// New returns a Harness whose Otx records every signal in memory.
//
// Options are applied after the recorders, so an option may replace any of
// them; the corresponding field of the Harness then no longer reflects what
// the Otx uses.
//
// The Otx is shut down through [testing.TB.Cleanup], which flushes the
// providers and fails the test if shutting down reports an error.
func New(tb testing.TB, opts ...otx.Option) *Harness {
	tb.Helper()

	spans := tracetest.NewSpanRecorder()
	logs := &otxmem.LogExporter{}
	reader := sdkmetric.NewManualReader()

	h := &Harness{
		Spans:  spans,
		Logs:   logs,
		Reader: reader,

		TracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)),
		MeterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		LoggerProvider: sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs))),

		tb: tb,
	}

	h.Otx = otx.New(append([]otx.Option{
		otx.WithTracerProvider(h.TracerProvider),
		otx.WithMeterProvider(h.MeterProvider),
		otx.WithLoggerProvider(h.LoggerProvider),
	}, opts...)...)

	tb.Cleanup(func() {
		if err := h.Otx.Shutdown(context.Background()); err != nil {
			tb.Errorf("otxtest: shutdown: %v", err)
		}
	})

	return h
}

// Into returns a copy of ctx carrying the Otx of this Harness.
func (h *Harness) Into(ctx context.Context) context.Context {
	return otx.Into(ctx, h.Otx)
}

// Ended returns the spans that have ended so far, in the order they ended.
func (h *Harness) Ended() []sdktrace.ReadOnlySpan {
	return h.Spans.Ended()
}

// Records returns the log records exported so far, in order.
func (h *Harness) Records() []sdklog.Record {
	return h.Logs.Records()
}

// Collect gathers the metrics recorded so far. It fails the test if the reader
// reports an error, so the result can be used directly.
//
// It reports that failure with [testing.TB.Fatalf], which the testing package
// requires be called from the goroutine running the test. Call it from there,
// as with any other assertion.
func (h *Harness) Collect(ctx context.Context) metricdata.ResourceMetrics {
	h.tb.Helper()

	rm := metricdata.ResourceMetrics{}
	if err := h.Reader.Collect(ctx, &rm); err != nil {
		h.tb.Fatalf("otxtest: collect metrics: %v", err)
	}

	return rm
}

// Reset discards the spans and log records recorded so far. Metrics are pulled
// rather than accumulated, so there is nothing of theirs to discard.
func (h *Harness) Reset() {
	h.Spans.Reset()
	h.Logs.Reset()
}
