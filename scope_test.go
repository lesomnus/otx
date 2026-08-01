package otx_test

import (
	"log/slog"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// scopeFixture wires an Otx to a span recorder, an in-memory log exporter and a
// recording meter provider so that the instrumentation scope can be read back
// off all four instruments - tracer, meter, logger and slog handler - at once.
// Three of them carry the scope into the exported telemetry; the meter is the
// odd one out, since this module has no metric SDK, so what it was asked for is
// read off the stub instead.
type scopeFixture struct {
	spans *tracetest.SpanRecorder
	logs  *otxmem.LogExporter
	meter *stubMeterProvider
	otx   *otx.Otx
}

func newScopeFixture(t *testing.T, opts ...otx.Option) *scopeFixture {
	t.Helper()

	f := &scopeFixture{
		spans: tracetest.NewSpanRecorder(),
		logs:  &otxmem.LogExporter{},
		meter: &stubMeterProvider{},
	}

	f.otx = otx.New(append([]otx.Option{
		otx.WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(f.spans))),
		otx.WithMeterProvider(f.meter),
		otx.WithLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(f.logs)))),
	}, opts...)...)

	return f
}

// emit produces one span, one OpenTelemetry log record and one slog record.
func (f *scopeFixture) emit(t *testing.T) {
	t.Helper()

	_, span := f.otx.TraceStart(t.Context(), "op")
	span.End()

	f.otx.Logger().Emit(t.Context(), logRecord("emitted"))
	slog.New(f.otx.SlogHandler()).InfoContext(t.Context(), "bridged")

	require.Len(t, f.spans.Ended(), 1)
	require.Equal(t, 2, f.logs.Len(), "the direct record and the bridged one")
}

// requireScope asserts that every instrument was created with the same scope.
func (f *scopeFixture) requireScope(t *testing.T, name string, version string, schema_url string) {
	t.Helper()

	span_scope := f.spans.Ended()[0].InstrumentationScope()
	require.Equal(t, name, span_scope.Name, "tracer name")
	require.Equal(t, version, span_scope.Version, "tracer version")
	require.Equal(t, schema_url, span_scope.SchemaURL, "tracer schema url")

	require.Equal(t, name, f.meter.scope_name, "meter name")
	require.Equal(t, version, f.meter.scope_cfg.InstrumentationVersion(), "meter version")
	require.Equal(t, schema_url, f.meter.scope_cfg.SchemaURL(), "meter schema url")

	for i, label := range []string{"logger", "slog handler"} {
		got := f.logs.Records()[i].InstrumentationScope()
		require.Equal(t, name, got.Name, label+" name")
		require.Equal(t, version, got.Version, label+" version")
		require.Equal(t, schema_url, got.SchemaURL, label+" schema url")
	}
}

func TestScope(t *testing.T) {
	t.Run("every field configured reaches all four instruments", func(t *testing.T) {
		f := newScopeFixture(t,
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v7.0.0"),
			otx.WithScopeSchemaURL("https://example.test/v7"),
		)
		f.emit(t)

		f.requireScope(t, "app", "v7.0.0", "https://example.test/v7")
	})
	t.Run("the default scope is this module at no version", func(t *testing.T) {
		// The version comes from the build info of the running binary. A test
		// binary of this module reports Main.Version "(devel)", which is a
		// placeholder rather than a version, so it is reported as no version at
		// all instead of being leaked into every exported span and record.
		f := newScopeFixture(t)
		f.emit(t)

		f.requireScope(t, otx.Scope, "", "")
	})
	t.Run("only the field given is overridden", func(t *testing.T) {
		f := newScopeFixture(t, otx.WithScopeSchemaURL("https://example.test/only"))
		f.emit(t)

		f.requireScope(t, otx.Scope, "", "https://example.test/only")
	})
	t.Run("the last of a repeated scope option wins", func(t *testing.T) {
		f := newScopeFixture(t,
			otx.WithScopeName("first"),
			otx.WithScopeVersion("v1"),
			otx.WithScopeSchemaURL("https://example.test/first"),
			otx.WithScopeName("last"),
			otx.WithScopeVersion("v2"),
			otx.WithScopeSchemaURL("https://example.test/last"),
		)
		f.emit(t)

		f.requireScope(t, "last", "v2", "https://example.test/last")
	})
	t.Run("an empty name keeps what is there while an empty version clears it", func(t *testing.T) {
		// The asymmetry is deliberate and easy to lose: a name is mandatory, so
		// "" means "no opinion", whereas "" is the only way to ask for a scope
		// with no version. Checked on every signal, not just on traces.
		f := newScopeFixture(t,
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v1"),
			otx.WithScopeSchemaURL("https://example.test/v1"),

			otx.WithScopeName(""),
			otx.WithScopeVersion(""),
			otx.WithScopeSchemaURL(""),
		)
		f.emit(t)

		f.requireScope(t, "app", "", "")
	})
}
