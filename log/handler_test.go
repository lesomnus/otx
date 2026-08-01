package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// call is a single invocation of a [recorder].
type call struct {
	ctx    context.Context
	level  slog.Level
	record slog.Record

	// State of the handler that took the call, so that a test can tell that a
	// binding survived WithAttrs and WithGroup.
	attrs  []slog.Attr
	groups []string
}

// sink collects the calls a [recorder] received. Every handler derived from a
// recorder with WithAttrs or WithGroup shares the sink of the handler it came
// from, so a test can assert on a derived handler it never held.
type sink struct {
	mu      sync.Mutex
	enabled []call
	handled []call
}

func (s *sink) addEnabled(c call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = append(s.enabled, c)
}

func (s *sink) addHandled(c call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handled = append(s.handled, c)
}

// recorder is a [log/slog.Handler] that records what it was asked to do.
type recorder struct {
	sink     *sink
	err      error
	disabled bool
	attrs    []slog.Attr
	groups   []string
}

func newRecorder() *recorder {
	return &recorder{sink: &sink{}}
}

// Enabled reports true unless the recorder was deliberately disabled, and
// records the call either way.
//
// The default must be true: a handler that reports false is never asked to
// Handle anything, so every assertion about Handle would hold vacuously. Only
// the two subtests that are about the verdict itself set disabled, and they
// assert that Handle was *not* reached rather than that it was. Tests assert
// on the number of calls recorded here and in Handle for the same reason.
func (h *recorder) Enabled(ctx context.Context, level slog.Level) bool {
	h.sink.addEnabled(call{ctx: ctx, level: level, attrs: h.attrs, groups: h.groups})
	return !h.disabled
}

func (h *recorder) Handle(ctx context.Context, record slog.Record) error {
	h.sink.addHandled(call{ctx: ctx, level: record.Level, record: record.Clone(), attrs: h.attrs, groups: h.groups})
	return h.err
}

func (h *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recorder{
		sink:     h.sink,
		err:      h.err,
		disabled: h.disabled,
		attrs:    append(slices.Clone(h.attrs), attrs...),
		groups:   h.groups,
	}
}

func (h *recorder) WithGroup(name string) slog.Handler {
	return &recorder{
		sink:     h.sink,
		err:      h.err,
		disabled: h.disabled,
		attrs:    h.attrs,
		groups:   append(slices.Clone(h.groups), name),
	}
}

// EnabledCalls returns the calls Enabled took, on this handler and on every
// handler derived from it.
func (h *recorder) EnabledCalls() []call {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()

	return slices.Clone(h.sink.enabled)
}

// HandleCalls returns the calls Handle took, on this handler and on every
// handler derived from it.
func (h *recorder) HandleCalls() []call {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()

	return slices.Clone(h.sink.handled)
}

var _ slog.Handler = (*recorder)(nil)

type markKey struct{}

// mark returns a context that is not the same value as any other, so that "the
// handler got exactly this one" can be told from "the handler got something
// that happens to look alike".
func mark(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, markKey{}, v)
}

// requireSameCtx asserts that two contexts are the very same value.
//
// require.Same is not usable here: context.Background and context.TODO are
// struct values, not pointers.
func requireSameCtx(t *testing.T, expected context.Context, actual context.Context) {
	t.Helper()
	require.True(t, expected == actual, "expected context %#v but was %#v", expected, actual)
}

// requireOnlyHandle asserts that the wrapped handler was actually reached,
// exactly once, and returns the call. Every test that says anything about
// Handle goes through it so that the suite cannot go vacuous.
func requireOnlyHandle(t *testing.T, h *recorder) call {
	t.Helper()

	calls := h.HandleCalls()
	require.Len(t, calls, 1, "the wrapped handler was not reached")

	return calls[0]
}

// requireOnlyEnabled is [requireOnlyHandle] for Enabled.
func requireOnlyEnabled(t *testing.T, h *recorder) call {
	t.Helper()

	calls := h.EnabledCalls()
	require.Len(t, calls, 1, "the wrapped handler was not asked whether it is enabled")

	return calls[0]
}

// attrsOf returns the attributes of a record, in order, as "key=value".
func attrsOf(record slog.Record) []string {
	attrs := make([]string, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr.String())
		return true
	})

	return attrs
}

// valuesOf returns the value of every attribute of record named key, so that a
// test can tell "recorded once" from "recorded again on top of a stale one".
func valuesOf(record slog.Record, key string) []string {
	vs := []string{}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			vs = append(vs, attr.Value.String())
		}
		return true
	})

	return vs
}

// namesOf returns the given attributes as "key=value".
func namesOf(attrs []slog.Attr) []string {
	vs := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		vs = append(vs, attr.String())
	}

	return vs
}

func newTracerProvider(t *testing.T) *sdktrace.TracerProvider {
	t.Helper()

	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	return provider
}

// startSpan starts a real, sampled span so that the ids the handler records
// are the ones an exporter would see.
func startSpan(t *testing.T, ctx context.Context, name string) (context.Context, trace.Span) {
	t.Helper()

	return startSpanOn(t, newTracerProvider(t), ctx, name)
}

func startSpanOn(t *testing.T, provider *sdktrace.TracerProvider, ctx context.Context, name string) (context.Context, trace.Span) {
	t.Helper()

	ctx, span := provider.Tracer("test").Start(ctx, name)
	t.Cleanup(func() { span.End() })
	require.True(t, span.SpanContext().IsValid())

	return ctx, span
}

func traceIDOf(span trace.Span) string {
	return span.SpanContext().TraceID().String()
}

func spanIDOf(span trace.Span) string {
	return span.SpanContext().SpanID().String()
}

func newRecord(msg string) slog.Record {
	return slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
}

// decodeJSON returns the single record a [log/slog.JSONHandler] wrote, with
// the timestamp dropped, and empties the buffer.
func decodeJSON(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	entry := map[string]any{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry), "not a single JSON record: %q", buf.String())
	delete(entry, slog.TimeKey)
	buf.Reset()

	return entry
}

func TestKeys(t *testing.T) {
	t.Run("the ids are recorded under the documented keys", func(t *testing.T) {
		// A plain slog handler is the reason these exist, so the strings
		// themselves are the contract.
		require.Equal(t, "trace_id", log.TraceIDKey)
		require.Equal(t, "span_id", log.SpanIDKey)
	})
}

func TestWithContext(t *testing.T) {
	t.Run("the bound context reaches Handle of the wrapped handler", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, "foo", c.record.Message)
	})
	t.Run("the bound context reaches Enabled of the wrapped handler", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).Info("foo")

		c := requireOnlyEnabled(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, slog.LevelInfo, c.level)
	})
	t.Run("Enabled asked directly with the empty context gets the bound one", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		ok := log.WithContext(ctx, h).Enabled(context.Background(), slog.LevelWarn)
		require.True(t, ok)

		c := requireOnlyEnabled(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, slog.LevelWarn, c.level)
	})
	t.Run("the verdict of the wrapped handler is what Enabled reports", func(t *testing.T) {
		h := newRecorder()
		h.disabled = true
		ctx := mark(t.Context(), "bound")

		ok := log.WithContext(ctx, h).Enabled(context.Background(), slog.LevelError)
		require.False(t, ok, "the verdict was not taken from the wrapped handler")

		c := requireOnlyEnabled(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, slog.LevelError, c.level)
	})
	t.Run("a record is dropped if the wrapped handler is not enabled", func(t *testing.T) {
		h := newRecorder()
		h.disabled = true
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).Info("foo")

		requireSameCtx(t, ctx, requireOnlyEnabled(t, h).ctx)
		require.Empty(t, h.HandleCalls(), "the record was handled by a handler that reported it is not enabled")
	})
	t.Run("the level of the wrapped handler decides", func(t *testing.T) {
		buf := &bytes.Buffer{}
		ctx, span := startSpan(t, t.Context(), "op")

		l := slog.New(log.WithContext(ctx, slog.NewJSONHandler(buf, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		})))

		l.Info("dropped")
		require.Empty(t, buf.String(), "a record below the level of the wrapped handler was written")

		l.Warn("kept")
		require.Equal(t, map[string]any{
			"level":        "WARN",
			"msg":          "kept",
			log.TraceIDKey: traceIDOf(span),
			log.SpanIDKey:  spanIDOf(span),
		}, decodeJSON(t, buf))
	})
	t.Run("an explicit context reaches both Enabled and Handle unchanged", func(t *testing.T) {
		h := newRecorder()
		ctx_bound := mark(t.Context(), "bound")
		ctx_call := mark(t.Context(), "call")

		slog.New(log.WithContext(ctx_bound, h)).InfoContext(ctx_call, "foo")

		requireSameCtx(t, ctx_call, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx_call, requireOnlyHandle(t, h).ctx)
	})
	t.Run("context.Background given explicitly is substituted", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).InfoContext(context.Background(), "foo")

		requireSameCtx(t, ctx, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
	})
	t.Run("context.TODO is substituted like context.Background", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).InfoContext(context.TODO(), "foo")

		requireSameCtx(t, ctx, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
	})
	t.Run("a nil context is substituted", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		var ctx_nil context.Context
		bound := log.WithContext(ctx, h)
		require.True(t, bound.Enabled(ctx_nil, slog.LevelInfo))
		require.NoError(t, bound.Handle(ctx_nil, newRecord("foo")))

		requireSameCtx(t, ctx, requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
	})
	t.Run("a handler bound to a nil context forwards what it is given", func(t *testing.T) {
		h := newRecorder()

		var ctx_nil context.Context
		bound := log.WithContext(ctx_nil, h)
		require.True(t, bound.Enabled(context.Background(), slog.LevelInfo))
		require.NoError(t, bound.Handle(context.Background(), newRecord("foo")))

		requireSameCtx(t, context.Background(), requireOnlyEnabled(t, h).ctx)
		requireSameCtx(t, context.Background(), requireOnlyHandle(t, h).ctx)
	})
	t.Run("records carry the ids of the active span", func(t *testing.T) {
		h := newRecorder()
		ctx, span := startSpan(t, t.Context(), "op")

		slog.New(log.WithContext(ctx, h)).Info("foo", slog.String("k", "v"))

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{
			"k=v",
			log.TraceIDKey + "=" + traceIDOf(span),
			log.SpanIDKey + "=" + spanIDOf(span),
		}, attrsOf(c.record))
	})
	t.Run("records carry no ids if there is no span", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).Info("foo", slog.String("k", "v"))

		c := requireOnlyHandle(t, h)
		require.Equal(t, []string{"k=v"}, attrsOf(c.record))
		require.Empty(t, valuesOf(c.record, log.TraceIDKey))
		require.Empty(t, valuesOf(c.record, log.SpanIDKey))
	})
	t.Run("records carry no ids if the span context is invalid", func(t *testing.T) {
		h := newRecorder()
		ctx := trace.ContextWithSpanContext(t.Context(), trace.SpanContext{})

		slog.New(log.WithContext(ctx, h)).Info("foo")

		c := requireOnlyHandle(t, h)
		require.Empty(t, attrsOf(c.record))
	})
	t.Run("the ids are those of the context the record is handled with", func(t *testing.T) {
		h := newRecorder()
		ctx_bound, span_bound := startSpan(t, t.Context(), "bound")
		ctx_call, span_call := startSpan(t, t.Context(), "call")

		slog.New(log.WithContext(ctx_bound, h)).InfoContext(ctx_call, "foo")

		c := requireOnlyHandle(t, h)
		require.Equal(t, []string{spanIDOf(span_call)}, valuesOf(c.record, log.SpanIDKey))
		require.NotEqual(t, spanIDOf(span_bound), spanIDOf(span_call))
	})
	t.Run("re-binding replaces the context instead of stacking wrappers", func(t *testing.T) {
		h := newRecorder()
		ctx_1, _ := startSpan(t, t.Context(), "first")
		ctx_2, span_2 := startSpan(t, t.Context(), "second")

		slog.New(log.WithContext(ctx_2, log.WithContext(ctx_1, h))).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx_2, c.ctx)

		// A stacked wrapper would add a second pair of ids.
		require.Equal(t, []string{traceIDOf(span_2)}, valuesOf(c.record, log.TraceIDKey))
		require.Equal(t, []string{spanIDOf(span_2)}, valuesOf(c.record, log.SpanIDKey))
		require.Equal(t, 2, c.record.NumAttrs())
	})
	t.Run("re-binding yields what binding the inner handler directly would", func(t *testing.T) {
		h := newRecorder()
		ctx_1 := mark(t.Context(), "first")
		ctx_2 := mark(t.Context(), "second")

		rebound := log.WithContext(ctx_2, log.WithContext(ctx_1, h))
		direct := log.WithContext(ctx_2, h)

		// The wrapper holds nothing but the wrapped handler and the context, so
		// it compares equal only if re-binding really replaced the context
		// instead of wrapping the wrapper.
		require.True(t, rebound == direct, "re-binding stacked another wrapper")
	})
	t.Run("re-binding leaves the handler it was given alone", func(t *testing.T) {
		h := newRecorder()
		ctx_1 := mark(t.Context(), "first")
		ctx_2 := mark(t.Context(), "second")

		bound_1 := log.WithContext(ctx_1, h)
		bound_2 := log.WithContext(ctx_2, bound_1)

		slog.New(bound_1).Info("one")
		slog.New(bound_2).Info("two")

		calls := h.HandleCalls()
		require.Len(t, calls, 2)
		requireSameCtx(t, ctx_1, calls[0].ctx)
		requireSameCtx(t, ctx_2, calls[1].ctx)
	})
	t.Run("WithAttrs preserves the binding", func(t *testing.T) {
		h := newRecorder()
		ctx, span := startSpan(t, t.Context(), "op")

		slog.New(log.WithContext(ctx, h)).With(slog.String("a", "b")).Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{"a=b"}, namesOf(c.attrs))
		require.Equal(t, []string{spanIDOf(span)}, valuesOf(c.record, log.SpanIDKey))
	})
	t.Run("WithGroup preserves the binding", func(t *testing.T) {
		h := newRecorder()
		ctx, span := startSpan(t, t.Context(), "op")

		slog.New(log.WithContext(ctx, h)).WithGroup("g").Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{"g"}, c.groups)

		// The ids are record attributes, so an open group takes them in like
		// any other; the wrapped handler is the one that nests them.
		require.Equal(t, []string{spanIDOf(span)}, valuesOf(c.record, log.SpanIDKey))
	})
	t.Run("WithAttrs and WithGroup keep the binding through a chain", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		slog.New(log.WithContext(ctx, h)).
			With(slog.String("a", "b")).
			WithGroup("g").
			With(slog.Int("n", 1)).
			Info("foo")

		c := requireOnlyHandle(t, h)
		requireSameCtx(t, ctx, c.ctx)
		require.Equal(t, []string{"a=b", "n=1"}, namesOf(c.attrs))
		require.Equal(t, []string{"g"}, c.groups)
	})
	t.Run("WithAttrs leaves the handler it was called on alone", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		bound := log.WithContext(ctx, h)
		derived := bound.WithAttrs([]slog.Attr{slog.String("a", "b")})

		require.NoError(t, derived.Handle(context.Background(), newRecord("derived")))
		require.NoError(t, bound.Handle(context.Background(), newRecord("bound")))

		calls := h.HandleCalls()
		require.Len(t, calls, 2)
		require.Equal(t, []string{"a=b"}, namesOf(calls[0].attrs))
		require.Empty(t, calls[1].attrs, "the attributes leaked into the handler they were derived from")
		requireSameCtx(t, ctx, calls[1].ctx)
	})
	t.Run("WithGroup leaves the handler it was called on alone", func(t *testing.T) {
		h := newRecorder()
		ctx := mark(t.Context(), "bound")

		bound := log.WithContext(ctx, h)
		derived := bound.WithGroup("g")

		require.NoError(t, derived.Handle(context.Background(), newRecord("derived")))
		require.NoError(t, bound.Handle(context.Background(), newRecord("bound")))

		calls := h.HandleCalls()
		require.Len(t, calls, 2)
		require.Equal(t, []string{"g"}, calls[0].groups)
		require.Empty(t, calls[1].groups, "the group leaked into the handler it was derived from")
		requireSameCtx(t, ctx, calls[1].ctx)
	})
	t.Run("the ids are nested in an open group", func(t *testing.T) {
		buf := &bytes.Buffer{}
		ctx, span := startSpan(t, t.Context(), "op")

		slog.New(log.WithContext(ctx, slog.NewJSONHandler(buf, nil))).
			WithGroup("g").
			Info("foo", slog.String("k", "v"))

		entry := decodeJSON(t, buf)
		require.NotContains(t, entry, log.TraceIDKey, "the ids escaped the open group")
		require.Equal(t, map[string]any{
			"k":            "v",
			log.TraceIDKey: traceIDOf(span),
			log.SpanIDKey:  spanIDOf(span),
		}, entry["g"], "the ids are not in the open group: %s", buf.String())
	})
	t.Run("the ids of a record already handled are not overwritten by a later call", func(t *testing.T) {
		ctx_a, span_a := startSpan(t, t.Context(), "a")
		ctx_b, span_b := startSpan(t, t.Context(), "b")
		require.NotEqual(t, spanIDOf(span_a), spanIDOf(span_b))

		// A record keeps its first attributes inline and the rest in a slice
		// that has spare capacity for some sizes. Handing the same record to
		// two bound handlers is what a fan-out handler does; if either appended
		// the ids to the record it was given instead of to a clone of it, both
		// would write through the same array, which log/slog notices and marks
		// with an extra "!BUG" attribute. Which size triggers it is an
		// allocation detail, so every size up to well past the inline array is
		// tried.
		for n := range 16 {
			h := newRecorder()
			bound_a := log.WithContext(ctx_a, h)
			bound_b := log.WithContext(ctx_b, h)

			record := newRecord("foo")
			for i := range n {
				record.AddAttrs(slog.Int("i", i))
			}

			require.NoError(t, bound_a.Handle(context.Background(), record))
			require.NoError(t, bound_b.Handle(context.Background(), record))

			calls := h.HandleCalls()
			require.Len(t, calls, 2)
			require.Equal(t, []string{spanIDOf(span_a)}, valuesOf(calls[0].record, log.SpanIDKey),
				"with %d attributes the ids of the first call are wrong", n)
			require.Equal(t, []string{spanIDOf(span_b)}, valuesOf(calls[1].record, log.SpanIDKey),
				"with %d attributes the ids of the second call are wrong", n)
			for i, c := range calls {
				require.Equal(t, n+2, c.record.NumAttrs(),
					"with %d attributes call %d got %v", n, i, attrsOf(c.record))
			}
			require.Equal(t, n, record.NumAttrs(), "with %d attributes the record of the caller grew", n)
		}
	})
	t.Run("a bound handler is safe for concurrent use", func(t *testing.T) {
		h := newRecorder()
		ctx, span := startSpan(t, t.Context(), "op")
		l := slog.New(log.WithContext(ctx, h))

		wg := sync.WaitGroup{}
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.With(slog.Int("n", i)).WithGroup("g").Info("foo")
			}()
		}
		wg.Wait()

		calls := h.HandleCalls()
		require.Len(t, calls, 8)
		for _, c := range calls {
			requireSameCtx(t, ctx, c.ctx)
			require.Equal(t, []string{"g"}, c.groups)
			require.Equal(t, []string{spanIDOf(span)}, valuesOf(c.record, log.SpanIDKey))
		}
	})
	t.Run("the error of the wrapped handler is returned", func(t *testing.T) {
		h := newRecorder()
		h.err = errors.New("nope")
		ctx, _ := startSpan(t, t.Context(), "op")

		err := log.WithContext(ctx, h).Handle(context.Background(), newRecord("foo"))
		require.ErrorIs(t, err, h.err)
		requireSameCtx(t, ctx, requireOnlyHandle(t, h).ctx)
	})
}
