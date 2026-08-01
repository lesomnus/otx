package tag_test

import (
	"log/slog"
	"testing"

	"github.com/lesomnus/otx/tag"
	"github.com/stretchr/testify/require"
)

func TestTitle(t *testing.T) {
	t.Run("key is the app.widget.name attribute", func(t *testing.T) {
		require.Equal(t, "app.widget.name", tag.TitleKey)
	})

	t.Run("holds the given title as a string", func(t *testing.T) {
		a := tag.Title("foo")
		require.Equal(t, tag.TitleKey, a.Key)
		require.Equal(t, slog.KindString, a.Value.Kind())
		require.Equal(t, "foo", a.Value.String())
	})

	t.Run("holds an empty title as an empty string", func(t *testing.T) {
		a := tag.Title("")
		require.Equal(t, tag.TitleKey, a.Key)
		require.Equal(t, slog.KindString, a.Value.Kind())
		require.Empty(t, a.Value.String())
	})
}
