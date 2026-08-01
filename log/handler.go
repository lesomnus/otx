package log

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceIDKey and SpanIDKey are the attribute keys under which [WithContext]
// records the active span.
//
// These are convenience keys, not semantic conventions: the OpenTelemetry log
// data model carries trace correlation in dedicated record fields, which the
// SDK fills in from the context on its own. They exist so that a plain
// [log/slog] handler - a text or JSON handler writing to stderr in local
// development - still shows the correlation.
const (
	TraceIDKey = "trace_id"
	SpanIDKey  = "span_id"
)

// handler pins a context to the wrapped handler so that the context-less
// [log/slog.Logger] methods still reach it with a real context, and records
// the active span on every record.
type handler struct {
	slog.Handler
	ctx context.Context
}

// WithContext returns h bound to ctx.
//
// [log/slog.Logger.Info] and its siblings pass [context.Background] to the
// handler, which loses everything a context carries: the active span, the
// [github.com/lesomnus/otx.Otx], and any value a downstream processor filters
// on. The returned handler substitutes ctx whenever it is handed one of the
// two empty contexts, [context.Background] or [context.TODO], so that
//
//	log.From(ctx).Info("hello")
//
// behaves like InfoContext(ctx, "hello"). A caller that deliberately passes
// [context.Background] to InfoContext gets ctx as well; there is no way to
// tell the two apart, and a logger obtained from [From] is bound to its
// context by construction.
//
// Re-binding an already bound handler replaces the context rather than
// stacking another wrapper, so repeated [From] calls along a call chain do not
// accumulate.
func WithContext(ctx context.Context, h slog.Handler) slog.Handler {
	if h_, ok := h.(handler); ok {
		h_.ctx = ctx
		return h_
	}

	return handler{
		Handler: h,
		ctx:     ctx,
	}
}

// Enabled is overridden alongside Handle so that both see the same context.
// The slog contract is that the context given to Enabled is the one the record
// will be handled with, which is what lets a context-aware processor decide
// whether to drop a record.
func (h handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(h.resolve(ctx), level)
}

func (h handler) Handle(ctx context.Context, record slog.Record) error {
	ctx = h.resolve(ctx)

	// Resolved per record rather than bound with Logger.With, so that the ids
	// are those of the span active at the moment of the call, and so that a
	// logger stashed with Into and read back with From cannot accumulate the
	// ids of every span it passed through.
	//
	// The attributes land wherever the wrapped handler is: if it was given an
	// open group with WithGroup, they are nested in it like any other record
	// attribute.
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		record = record.Clone()
		record.AddAttrs(
			slog.String(TraceIDKey, span.TraceID().String()),
			slog.String(SpanIDKey, span.SpanID().String()),
		)
	}

	return h.Handler.Handle(ctx, record)
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Handler = h.Handler.WithAttrs(attrs)
	return h
}

func (h handler) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	return h
}

// resolve substitutes the bound context for the empty ones slog synthesises.
func (h handler) resolve(ctx context.Context) context.Context {
	if h.ctx == nil {
		return ctx
	}
	if ctx == nil || ctx == context.Background() || ctx == context.TODO() {
		return h.ctx
	}

	return ctx
}
