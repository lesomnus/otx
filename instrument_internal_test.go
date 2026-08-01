package otx

// The rest of instrument.go is tested from otx_test, through the eight
// exported constructors. This file exists for the one branch none of them can
// reach: instrumentKind is unexported and every value an exported constructor
// passes has a name of its own, so only a value made inside the package falls
// through to the generic one.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstrumentKindString(t *testing.T) {
	t.Run("every kind a constructor uses names itself", func(t *testing.T) {
		// The names go into the error report of a rejected instrument, which is
		// all the caller of a constructor that swallows its error ever sees, so
		// each has to say which of the eight was asked for.
		want := map[instrumentKind]string{
			kindInt64Counter:         "Int64Counter",
			kindInt64UpDownCounter:   "Int64UpDownCounter",
			kindInt64Histogram:       "Int64Histogram",
			kindInt64Gauge:           "Int64Gauge",
			kindFloat64Counter:       "Float64Counter",
			kindFloat64UpDownCounter: "Float64UpDownCounter",
			kindFloat64Histogram:     "Float64Histogram",
			kindFloat64Gauge:         "Float64Gauge",
		}
		require.Len(t, want, 8)

		seen := map[string]instrumentKind{}
		for kind, name := range want {
			require.Equal(t, name, kind.String())

			_, ok := seen[name]
			require.False(t, ok, "%s is the name of two kinds", name)
			seen[name] = kind
		}
	})
	t.Run("a kind with no name falls back to a generic one", func(t *testing.T) {
		// A ninth kind added to the constants but not to String would report
		// itself as "instrument" rather than as a bare number.
		require.Equal(t, "instrument", instrumentKind(200).String())
	})
}
