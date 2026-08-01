package otx

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

// versionOf is tested from inside the package because it takes a
// *debug.BuildInfo, which no exported entry point accepts. The branch that
// matters most is the one this module's own tests can never reach: every
// consumer of otx finds it under Deps, not Main.
func TestVersionOf(t *testing.T) {
	t.Run("the version is taken from the main module when otx is it", func(t *testing.T) {
		v := versionOf(&debug.BuildInfo{
			Main: debug.Module{Path: Scope, Version: "v1.2.3"},
		})
		require.Equal(t, "v1.2.3", v)
	})
	t.Run("the version is taken from the dependencies when otx is one", func(t *testing.T) {
		v := versionOf(&debug.BuildInfo{
			Main: debug.Module{Path: "github.com/me/my-service", Version: "v0.1.0"},
			Deps: []*debug.Module{
				{Path: "github.com/other/thing", Version: "v9.9.9"},
				{Path: Scope, Version: "v1.2.3"},
			},
		})
		require.Equal(t, "v1.2.3", v)
	})
	t.Run("a working tree reports no version", func(t *testing.T) {
		v := versionOf(&debug.BuildInfo{
			Main: debug.Module{Path: Scope, Version: "(devel)"},
		})
		require.Empty(t, v)
	})
	t.Run("a build that does not mention otx reports no version", func(t *testing.T) {
		v := versionOf(&debug.BuildInfo{
			Main: debug.Module{Path: "github.com/me/my-service", Version: "v0.1.0"},
			Deps: []*debug.Module{
				nil,
				{Path: "github.com/other/thing", Version: "v9.9.9"},
			},
		})
		require.Empty(t, v)
	})
	t.Run("the scope version of a working tree build is empty", func(t *testing.T) {
		// moduleVersion runs against the real build info of the test binary,
		// where this module is Main at "(devel)".
		require.Empty(t, moduleVersion())
	})
}
