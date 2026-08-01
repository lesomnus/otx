package log_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// exportedAttrs returns the attributes of an exported record by key.
func exportedAttrs(record sdklog.Record) map[string]otellog.Value {
	attrs := map[string]otellog.Value{}
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		attrs[kv.Key] = kv.Value
		return true
	})

	return attrs
}

// exportedValues returns the value of every attribute of an exported record
// named key, in order, so that "recorded once" can be told from "recorded
// again on top of a stale one".
func exportedValues(record sdklog.Record, key string) []string {
	vs := []string{}
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		if kv.Key == key {
			vs = append(vs, kv.Value.String())
		}
		return true
	})

	return vs
}

// newLoggerProvider returns a logger provider exporting into memory.
// globalLoggerExporter installs a logger provider as the OpenTelemetry global
// and returns what it exports.
//
// It is installed exactly once per process, and never shut down, because both
// halves of this are one-shot: the delegating global provider binds the loggers
// it has already handed out to the first provider set, and the Otx that
// otx.From falls back to is built once. Installing a second provider would
// leave the first one wired to everything, which is what made this test pass
// only on the first run under -count=2.
var globalLoggerExporter = sync.OnceValue(func() *otxmem.LogExporter {
	exporter := &otxmem.LogExporter{}
	logglobal.SetLoggerProvider(sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
	))

	return exporter
})

func newLoggerProvider(t *testing.T) (*sdklog.LoggerProvider, *otxmem.LogExporter) {
	t.Helper()

	exporter := &otxmem.LogExporter{}
	provider := sdklog.NewLoggerProvider(
		// Simple, not batch: the records must be readable the moment the call
		// that wrote them returns.
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)),
	)
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	return provider, exporter
}

func TestInto(t *testing.T) {
	t.Run("a nil logger leaves the context unchanged", func(t *testing.T) {
		ctx := mark(t.Context(), "given")
		requireSameCtx(t, ctx, log.Into(ctx, nil))
	})
	t.Run("a nil logger does not clear an already stashed one", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		ctx = log.Into(ctx, nil)
		log.From(ctx).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, "foo", c.record.Message)
	})
	t.Run("From on a context given a nil logger falls through to the Otx", func(t *testing.T) {
		logger_provider, exporter := newLoggerProvider(t)

		v := otx.New(otx.WithLoggerProvider(logger_provider))
		ctx := otx.Into(t.Context(), v)
		ctx = log.Into(ctx, nil)

		var l *slog.Logger
		require.NotPanics(t, func() { l = log.From(ctx) })
		require.NotNil(t, l)
		require.NotNil(t, l.Handler())
		l.Info("foo")

		records := exporter.Records()
		require.Len(t, records, 1)
		require.Equal(t, "foo", records[0].Body().AsString())
	})
	t.Run("the stashed logger is the one From uses", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		log.From(ctx).Info("foo")

		require.Len(t, h.HandleCalls(), 1)
	})
	t.Run("the logger stashed last is the one From uses", func(t *testing.T) {
		h_outer := newRecorder()
		h_inner := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h_outer))
		ctx = log.Into(ctx, slog.New(h_inner))
		log.From(ctx).Info("foo")

		requireSameCtx(t, ctx, requireOnlyHandle(t, h_inner).ctx)
		require.Empty(t, h_outer.HandleCalls(), "the shadowed logger was used")
	})
}

func TestFrom(t *testing.T) {
	t.Run("the context is what the stashed handler is given", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		log.From(ctx).Info("foo")

		requireSameCtx(t, ctx, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
	})
	t.Run("the context of the call site is used, not the one the logger was stashed with", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		ctx_child, cancel := context.WithCancel(ctx)
		defer cancel()

		log.From(ctx_child).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx_child, c.ctx)
		require.NotNil(t, c.ctx.Done(), "the child context lost its cancellation")
	})
	t.Run("an explicit context is passed through unchanged", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		ctx_call := mark(t.Context(), "call")
		log.From(ctx).InfoContext(ctx_call, "foo")

		requireSameCtx(t, ctx_call, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx_call, requireOnlyHandle(t, h).ctx)
	})
	t.Run("the attributes and groups of the stashed logger are kept", func(t *testing.T) {
		h := newRecorder()

		l := slog.New(h).With(slog.String("a", "b")).WithGroup("g")
		ctx := log.Into(t.Context(), l)
		log.From(ctx).Info("foo", slog.Int("n", 1))

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{"a=b"}, namesOf(c.attrs))
		require.Equal(t, []string{"g"}, c.groups)
		require.Equal(t, []string{"n=1"}, attrsOf(c.record))
	})
	t.Run("attributes added to the returned logger are kept", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		log.From(ctx).With(slog.String("a", "b")).WithGroup("g").Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{"a=b"}, namesOf(c.attrs))
		require.Equal(t, []string{"g"}, c.groups)
	})
	t.Run("records carry the ids of the span active at the call", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		ctx, span := startSpan(t, ctx, "op")
		log.From(ctx).Info("foo")

		c := requireOnlyHandle(t, h)
		require.Equal(t, []string{
			log.TraceIDKey + "=" + traceIDOf(span),
			log.SpanIDKey + "=" + spanIDOf(span),
		}, attrsOf(c.record))
	})
	t.Run("the ids are resolved per record, not baked into the stashed logger", func(t *testing.T) {
		h := newRecorder()
		provider := newTracerProvider(t)

		ctx := log.Into(t.Context(), slog.New(h))
		ctx_a, span_a := startSpanOn(t, provider, ctx, "a")

		// A logger read out and stashed back while span A is active must not
		// remember A: were the ids bound with Logger.With, every stash would
		// leave a pair behind.
		for range 3 {
			ctx_a = log.Into(ctx_a, log.From(ctx_a))
		}

		ctx_b, span_b := startSpanOn(t, provider, ctx_a, "b")
		log.From(ctx_b).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx_b, c.ctx)
		require.NotEqual(t, spanIDOf(span_a), spanIDOf(span_b))
		require.Equal(t, []string{spanIDOf(span_b)}, valuesOf(c.record, log.SpanIDKey))
		require.Equal(t, []string{traceIDOf(span_b)}, valuesOf(c.record, log.TraceIDKey))
		require.Equal(t, 2, c.record.NumAttrs(), "the ids accumulated")
		require.Empty(t, c.attrs, "the ids were bound to the handler instead of the record")
	})
	t.Run("stashing does not accumulate however often it is repeated", func(t *testing.T) {
		h := newRecorder()

		ctx := log.Into(t.Context(), slog.New(h))
		ctx, _ = startSpan(t, ctx, "op")

		for n := range 4 {
			ctx = log.Into(ctx, log.From(ctx))
			log.From(ctx).Info("foo")

			calls := h.HandleCalls()
			require.Len(t, calls, n+1)
			require.Equal(t, 2, calls[n].record.NumAttrs())
			require.Empty(t, calls[n].attrs)
			require.Empty(t, calls[n].groups)
		}
	})
	t.Run("a context that carries nothing yields a usable logger", func(t *testing.T) {
		ctx := mark(t.Context(), "empty")
		_, ok := otx.FromOK(ctx)
		require.False(t, ok)

		require.NotPanics(t, func() {
			l := log.From(ctx)
			require.NotNil(t, l)
			require.NotNil(t, l.Handler())
			l.Info("foo", slog.String("k", "v"))
			l.With(slog.Bool("b", true)).WithGroup("g").Error("bar")
		})
	})
	t.Run("the returned handler is the one of the Otx, bound to the context", func(t *testing.T) {
		for _, tc := range []struct {
			desc string
			ctx  func(t *testing.T) context.Context
		}{
			{
				desc: "from the context",
				ctx: func(t *testing.T) context.Context {
					logger_provider, _ := newLoggerProvider(t)
					return otx.Into(t.Context(), otx.New(otx.WithLoggerProvider(logger_provider)))
				},
			},
			{
				// The fallback Otx, which is shared, so its handler is the same
				// value on every call.
				desc: "from the globals",
				ctx: func(t *testing.T) context.Context {
					return mark(t.Context(), "empty")
				},
			},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				ctx := tc.ctx(t)

				// Equal only if From wrapped exactly the shared handler of the
				// Otx, exactly once, with exactly this context.
				require.True(t,
					log.From(ctx).Handler() == log.WithContext(ctx, otx.From(ctx).SlogHandler()),
					"the handler is not the one of the Otx bound to the context",
				)
			})
		}
	})
	t.Run("the logger provider of the Otx in the context is used", func(t *testing.T) {
		logger_provider, exporter := newLoggerProvider(t)
		tracer_provider := newTracerProvider(t)

		v := otx.New(
			otx.WithLoggerProvider(logger_provider),
			otx.WithTracerProvider(tracer_provider),
			otx.WithScopeName("test-scope"),
			otx.WithScopeVersion("v0.0.0-test"),
		)

		ctx := otx.Into(t.Context(), v)
		ctx, span := otx.TraceStart(ctx, "op")
		defer span.End()

		log.From(ctx).Info("hello", slog.String("k", "v"))

		records := exporter.Records()
		require.Len(t, records, 1)

		record := records[0]
		require.Equal(t, "hello", record.Body().AsString())
		require.Equal(t, otellog.SeverityInfo, record.Severity())
		require.Equal(t, slog.LevelInfo.String(), record.SeverityText())
		require.Equal(t, "test-scope", record.InstrumentationScope().Name)

		// Set by the SDK from the context handed to Emit, so they are only
		// there if the context of the call really reached it.
		require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
		require.Equal(t, span.SpanContext().SpanID(), record.SpanID())

		attrs := exportedAttrs(record)
		require.Len(t, attrs, 3)
		require.Equal(t, "v", attrs["k"].AsString())
		require.Equal(t, traceIDOf(span), attrs[log.TraceIDKey].AsString())
		require.Equal(t, spanIDOf(span), attrs[log.SpanIDKey].AsString())
	})
	t.Run("stashing the logger of an Otx back does not accumulate ids", func(t *testing.T) {
		logger_provider, exporter := newLoggerProvider(t)
		tracer_provider := newTracerProvider(t)

		v := otx.New(
			otx.WithLoggerProvider(logger_provider),
			otx.WithTracerProvider(tracer_provider),
		)

		ctx := otx.Into(t.Context(), v)
		ctx, span_outer := otx.TraceStart(ctx, "outer")
		defer span_outer.End()

		// The idiom this has to survive: every layer reads the logger out,
		// adds something and puts it back. The keys differ because the
		// OpenTelemetry log data model keys attributes uniquely, so a repeated
		// key would be collapsed by the bridge and hide an accumulation.
		for i := range 3 {
			ctx = log.Into(ctx, log.From(ctx).With(slog.Int(fmt.Sprintf("layer%d", i), i)))
		}

		ctx, span_inner := otx.TraceStart(ctx, "inner")
		defer span_inner.End()
		require.NotEqual(t, span_outer.SpanContext().SpanID(), span_inner.SpanContext().SpanID())

		log.From(ctx).Info("hello")

		records := exporter.Records()
		require.Len(t, records, 1)

		record := records[0]
		attrs := exportedAttrs(record)
		for i := range 3 {
			require.Equal(t, int64(i), attrs[fmt.Sprintf("layer%d", i)].AsInt64(), "a layer was lost")
		}
		require.Equal(t, []string{traceIDOf(span_inner)}, exportedValues(record, log.TraceIDKey))
		require.Equal(t, []string{spanIDOf(span_inner)}, exportedValues(record, log.SpanIDKey))
		require.Equal(t, 5, record.AttributesLen(), "an attribute was recorded more than once")
		require.Equal(t, span_inner.SpanContext().SpanID(), record.SpanID())
	})
	t.Run("a logger stashed over an Otx wins", func(t *testing.T) {
		h := newRecorder()
		logger_provider, exporter := newLoggerProvider(t)

		v := otx.New(otx.WithLoggerProvider(logger_provider))
		ctx := otx.Into(t.Context(), v)
		ctx = log.Into(ctx, slog.New(h))

		log.From(ctx).Info("foo")

		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
		require.Zero(t, exporter.Len())
	})
	t.Run("degrades to the global logger provider", func(t *testing.T) {
		exporter := globalLoggerExporter()

		// Other subtests may have logged into the global provider already.
		exporter.Reset()

		ctx := t.Context()
		_, ok := otx.FromOK(ctx)
		require.False(t, ok)

		ctx, span := startSpan(t, ctx, "op")

		// Info, not InfoContext: the span only reaches the bridge because the
		// handler is bound to the context.
		log.From(ctx).Info("hello", slog.String("k", "v"))

		records := exporter.Records()
		require.Len(t, records, 1)

		record := records[0]
		require.Equal(t, "hello", record.Body().AsString())
		require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
		require.Equal(t, span.SpanContext().SpanID(), record.SpanID())

		attrs := exportedAttrs(record)
		require.Len(t, attrs, 3)
		require.Equal(t, "v", attrs["k"].AsString())
		require.Equal(t, traceIDOf(span), attrs[log.TraceIDKey].AsString())
		require.Equal(t, spanIDOf(span), attrs[log.SpanIDKey].AsString())
	})
}
