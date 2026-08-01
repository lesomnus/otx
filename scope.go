package otx

import (
	"runtime/debug"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// scope is the instrumentation scope every instrument of an [Otx] is created
// with. The three signals take the same triple through three different option
// types, so it is kept in one place and converted per signal.
type scope struct {
	name       string
	version    string
	schema_url string
}

func (s scope) traceOpts() []trace.TracerOption {
	opts := []trace.TracerOption{}
	if s.version != "" {
		opts = append(opts, trace.WithInstrumentationVersion(s.version))
	}
	if s.schema_url != "" {
		opts = append(opts, trace.WithSchemaURL(s.schema_url))
	}

	return opts
}

func (s scope) metricOpts() []metric.MeterOption {
	opts := []metric.MeterOption{}
	if s.version != "" {
		opts = append(opts, metric.WithInstrumentationVersion(s.version))
	}
	if s.schema_url != "" {
		opts = append(opts, metric.WithSchemaURL(s.schema_url))
	}

	return opts
}

func (s scope) logOpts() []log.LoggerOption {
	opts := []log.LoggerOption{}
	if s.version != "" {
		opts = append(opts, log.WithInstrumentationVersion(s.version))
	}
	if s.schema_url != "" {
		opts = append(opts, log.WithSchemaURL(s.schema_url))
	}

	return opts
}

func (s scope) slogOpts(provider log.LoggerProvider) []otelslog.Option {
	opts := []otelslog.Option{otelslog.WithLoggerProvider(provider)}
	if s.version != "" {
		opts = append(opts, otelslog.WithVersion(s.version))
	}
	if s.schema_url != "" {
		opts = append(opts, otelslog.WithSchemaURL(s.schema_url))
	}

	return opts
}

// moduleVersion reports the version of this module as recorded in the build
// info, so that exported telemetry carries an instrumentation scope version
// instead of an empty string. It returns "" when the version is unknown, which
// includes builds from a working tree.
var moduleVersion = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	return versionOf(bi)
})

// versionOf picks this module's version out of the build info. It is separate
// from moduleVersion so that both branches can be exercised: within this
// module's own tests only the Main branch is ever taken, while every consumer
// of otx takes the Deps one.
func versionOf(bi *debug.BuildInfo) string {
	v := ""
	if bi.Main.Path == Scope {
		v = bi.Main.Version
	} else {
		for _, d := range bi.Deps {
			if d != nil && d.Path == Scope {
				v = d.Version
				break
			}
		}
	}

	// A working tree has no version. Reporting "(devel)" as the scope version
	// of exported telemetry would be noise.
	if v == "(devel)" {
		return ""
	}

	return v
}
