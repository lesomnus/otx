package tag_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/lesomnus/otx/tag"
	"github.com/stretchr/testify/require"
)

// TitleKey has to be a constant, not a variable, so that it can be used where
// a compile-time value is required - a case of a switch on an attribute key,
// for one. This declaration does not compile if that ever changes.
const title_key_is_constant = tag.TitleKey

// logJSON runs f against a logger writing JSON and returns the single record
// it produced, decoded.
func logJSON(t *testing.T, f func(l *slog.Logger)) map[string]any {
	t.Helper()

	buf := bytes.Buffer{}
	f(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Drop the built-ins so that only what the test logged remains.
			if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.MessageKey) {
				return slog.Attr{}
			}

			return a
		},
	})))

	rst := map[string]any{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rst))

	return rst
}

func TestTitleAttr(t *testing.T) {
	t.Run("the key is a compile-time constant", func(t *testing.T) {
		require.Equal(t, "app.widget.name", title_key_is_constant)
	})

	t.Run("equals the string attribute it stands for", func(t *testing.T) {
		require.True(t,
			tag.Title("foo").Equal(slog.String("app.widget.name", "foo")),
			"got %v", tag.Title("foo"),
		)
		require.False(t, tag.Title("foo").Equal(slog.String("app.widget.name", "bar")))
		require.False(t, tag.Title("foo").Equal(slog.String("title", "foo")))
	})

	t.Run("reaches a handler under its own key", func(t *testing.T) {
		out := logJSON(t, func(l *slog.Logger) {
			l.Info("hello", tag.Title("foo"))
		})
		require.Equal(t, map[string]any{"app.widget.name": "foo"}, out)
	})

	t.Run("is nested when the logger has an open group", func(t *testing.T) {
		// The key is not special-cased anywhere, so it nests like any other
		// attribute; a caller that wants it at the top level must not put it
		// in a group.
		out := logJSON(t, func(l *slog.Logger) {
			l.WithGroup("g").Info("hello", tag.Title("foo"))
		})
		require.Equal(t, map[string]any{"g": map[string]any{"app.widget.name": "foo"}}, out)
	})

	t.Run("carries the title verbatim", func(t *testing.T) {
		for _, title := range []string{
			"",
			" ",
			"a b c",
			"héllo/世界",
			`quote" and \backslash`,
			"a\nb",
		} {
			a := tag.Title(title)
			require.Equal(t, tag.TitleKey, a.Key)
			require.Equal(t, slog.KindString, a.Value.Kind())
			require.Equal(t, title, a.Value.String())

			out := logJSON(t, func(l *slog.Logger) {
				l.Info("hello", a)
			})
			require.Equal(t, map[string]any{tag.TitleKey: title}, out)
		}
	})

	t.Run("needs no resolving", func(t *testing.T) {
		// A string value is not a LogValuer, so a handler that skips Resolve
		// still sees the title.
		a := tag.Title("foo")
		require.True(t, a.Value.Equal(a.Value.Resolve()))
	})

	t.Run("two titles share one key so the innermost is the last written", func(t *testing.T) {
		// The reason the key lives here rather than being spelled out at each
		// call site: every Title collides with every other one by design.
		require.Equal(t, tag.Title("foo").Key, tag.Title("bar").Key)

		// A JSON handler does not deduplicate, so it writes both, the bound
		// one first. A reader taking the last occurrence - encoding/json, and
		// most log backends - therefore sees the one given at the call site.
		out := logJSON(t, func(l *slog.Logger) {
			l.With(tag.Title("outer")).Info("hello", tag.Title("inner"))
		})
		require.Equal(t, "inner", out[tag.TitleKey])
	})
}
