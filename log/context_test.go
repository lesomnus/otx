package log_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/lesomnus/otx/log"
	"github.com/stretchr/testify/require"
)

type ctxHoldHandler struct {
	slog.Handler
	ctx context.Context
}

func (h *ctxHoldHandler) Handle(ctx context.Context, r slog.Record) error {
	h.ctx = ctx
	return nil
}

func TestFrom(t *testing.T) {
	t.Run("context is forwarded", func(t *testing.T) {
		h := &ctxHoldHandler{
			Handler: slog.DiscardHandler,
			ctx:     nil,
		}

		ctx := log.Into(t.Context(), slog.New(h))
		ctx_child, _ := context.WithCancel(ctx)
		log.From(ctx_child).Info("foo")

		require.NotEqual(t, ctx, h.ctx)
		require.NotEqual(t, ctx_child, h.ctx)
	})
}
