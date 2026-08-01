package otx

// The rest of this package is tested from otx_test, through the exported API.
// This file exists for the one branch that has no exported path into it: the
// context key is unexported and Into refuses a nil *Otx, so a context carrying
// a nil *Otx can only be built from inside the package.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromOKNilValue(t *testing.T) {
	t.Run("a nil Otx under the key is treated as absent", func(t *testing.T) {
		// Into is the only writer today, and it screens nil out. The guard is
		// what keeps From's "never returns nil" promise from depending on that
		// remaining true, and without it every accessor would panic on a
		// context a future writer inside this package got wrong.
		ctx := context.WithValue(t.Context(), ctxKey{}, (*Otx)(nil))

		v, ok := FromOK(ctx)
		require.False(t, ok)
		require.Nil(t, v)

		require.Same(t, fallback(), From(ctx))
		require.NotNil(t, From(ctx).Tracer())

		require.ErrorIs(t, Start(ctx), ErrNoOtx)
		require.ErrorIs(t, Shutdown(ctx), ErrNoOtx)
		require.ErrorIs(t, ForceFlush(ctx), ErrNoOtx)
	})
	t.Run("a value of another type under the key is treated as absent", func(t *testing.T) {
		ctx := context.WithValue(t.Context(), ctxKey{}, "not an Otx")

		v, ok := FromOK(ctx)
		require.False(t, ok)
		require.Nil(t, v)
		require.Same(t, fallback(), From(ctx))
	})
	t.Run("a real Otx under the key is found", func(t *testing.T) {
		// The counterpart, so that the two subtests above cannot pass because
		// the key stopped matching anything at all.
		x := New()
		ctx := context.WithValue(t.Context(), ctxKey{}, x)

		v, ok := FromOK(ctx)
		require.True(t, ok)
		require.Same(t, x, v)
	})
}
