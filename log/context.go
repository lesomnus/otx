package log

import (
	"context"
	"log/slog"

	"github.com/lesomnus/otx"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

type ctxKey struct{}

func Into(ctx context.Context, v *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, v)
}

func From(ctx context.Context) *slog.Logger {
	var h slog.Handler

	v, ok := ctx.Value(ctxKey{}).(*slog.Logger)
	if ok {
		h = v.Handler()
	} else {
		lp := otx.Providers(ctx).Logger()
		h = otelslog.NewHandler(otx.Scope, otelslog.WithLoggerProvider(lp))
	}

	return slog.New(WithContext(ctx, h))
}
