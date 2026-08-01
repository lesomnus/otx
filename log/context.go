// Package log turns the OpenTelemetry logger carried by a
// [github.com/lesomnus/otx.Otx] into a [log/slog.Logger].
//
// The logger returned by [From] is bound to the context it came from, so the
// short methods carry the context anyway:
//
//	l := log.From(ctx)
//	l.Info("hello") // reaches the handler with ctx, not context.Background
//
// Every record it writes carries the ids of the span active at the moment of
// the call.
package log

import (
	"context"
	"log/slog"

	"github.com/lesomnus/otx"
)

type ctxKey struct{}

// Into returns a copy of ctx carrying v, which [From] then uses instead of the
// OpenTelemetry logger provider. Use it to attach attributes shared by
// everything logged downstream:
//
//	ctx = log.Into(ctx, log.From(ctx).With(slog.String("tenant", id)))
//
// A nil logger leaves ctx unchanged.
func Into(ctx context.Context, v *slog.Logger) context.Context {
	if v == nil {
		return ctx
	}

	return context.WithValue(ctx, ctxKey{}, v)
}

// From returns a [log/slog.Logger] bound to ctx.
//
// It uses the logger stashed by [Into] if there is one, and otherwise the
// OpenTelemetry logger provider of the [github.com/lesomnus/otx.Otx] carried
// by ctx, bridged with
// [go.opentelemetry.io/contrib/bridges/otelslog].
//
// A context that carries neither yields a logger backed by the OpenTelemetry
// global logger provider, which discards everything until one is installed.
func From(ctx context.Context) *slog.Logger {
	var h slog.Handler
	if v, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && v != nil {
		h = v.Handler()
	} else {
		h = otx.From(ctx).SlogHandler()
	}

	return slog.New(WithContext(ctx, h))
}
