package log

import (
	"context"
	"log/slog"
)

type handler struct {
	slog.Handler
	ctx context.Context
}

func WithContext(ctx context.Context, h slog.Handler) slog.Handler {
	h_, ok := h.(handler)
	if ok {
		h_.ctx = ctx
		return h_
	}

	return handler{
		Handler: h,
		ctx:     ctx,
	}
}

func (h handler) Handle(ctx context.Context, record slog.Record) error {
	if ctx == context.Background() {
		ctx = h.ctx
	}

	return h.Handler.Handle(ctx, record)
}
