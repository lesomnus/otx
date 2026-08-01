package otx_test

import (
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxmem"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestGlobals is the only test in this package that installs OpenTelemetry
// globals, and it must stay that way.
//
// otel.SetTracerProvider and log/global.SetLoggerProvider hand the new
// provider to the instruments already created from the delegating default
// exactly once per process; a second call replaces what the getters return but
// leaves every instrument already handed out pointing at the first one. So the
// process gets a single chance to prove the claim New makes - that a provider
// installed globally *after* New is still picked up - and spreading the
// mutation over several tests would make them depend on the order they run in.
//
// Everything installed here stays installed for the rest of the run. No other
// test asserts that the globals are unconfigured, and none of them shuts these
// providers down, which is itself asserted below.
var globals_installed atomic.Bool

func TestGlobals(t *testing.T) {
	// Once per process, for the reason above: on a second run the globals are
	// already delegating to the providers the first run installed, so nothing
	// this one installs would ever be reached and every assertion below would
	// be about the previous run's recorders.
	if globals_installed.Swap(true) {
		t.Skip("the OpenTelemetry globals can be installed only once per process")
	}

	ctx := t.Context()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	exporter := &otxmem.LogExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))

	// Built while the globals are still the delegating defaults.
	before := otx.New()
	require.Same(t, otel.GetTracerProvider(), before.Providers().Tracer())
	require.Same(t, logglobal.GetLoggerProvider(), before.Providers().Logger())

	_, span := before.TraceStart(ctx, "unconfigured")
	span.End()
	before.Logger().Emit(ctx, logRecord("unconfigured"))
	require.Empty(t, sr.Ended(), "nothing reaches an exporter that is not installed yet")
	require.Zero(t, exporter.Len())

	otel.SetTracerProvider(tp)
	logglobal.SetLoggerProvider(lp)

	t.Run("a tracer provider installed after New is still picked up", func(t *testing.T) {
		sr.Reset()

		_, span := before.TraceStart(ctx, "configured")
		span.End()

		require.Len(t, sr.Ended(), 1)
		require.Equal(t, "configured", sr.Ended()[0].Name())
		require.Equal(t, otx.Scope, sr.Ended()[0].InstrumentationScope().Name)
	})
	t.Run("a logger provider installed after New is still picked up", func(t *testing.T) {
		exporter.Reset()

		before.Logger().Emit(ctx, logRecord("configured"))

		require.Equal(t, 1, exporter.Len())
		require.Equal(t, "configured", exporter.Records()[0].Body().AsString())
		require.Equal(t, otx.Scope, exporter.Records()[0].InstrumentationScope().Name)
	})
	t.Run("the slog handler follows the global too", func(t *testing.T) {
		exporter.Reset()

		slog.New(before.SlogHandler()).InfoContext(ctx, "bridged")

		require.Equal(t, 1, exporter.Len())
		require.Equal(t, "bridged", exporter.Records()[0].Body().AsString())
	})
	t.Run("New now reads the installed providers directly", func(t *testing.T) {
		after := otx.New()
		require.Same(t, tp, after.Providers().Tracer())
		require.Same(t, lp, after.Providers().Logger())
	})
	t.Run("providers taken from the globals are not shut down", func(t *testing.T) {
		// The globals were not created by this Otx, so closing them would take
		// the telemetry pipeline of the whole process down with it - even
		// though both of them do have a Shutdown method.
		after := otx.New()
		require.NoError(t, after.ForceFlush(ctx))
		require.NoError(t, after.Shutdown(ctx))

		sr.Reset()
		exporter.Reset()

		_, span := after.TraceStart(ctx, "still alive")
		span.End()
		after.Logger().Emit(ctx, logRecord("still alive"))

		require.Len(t, sr.Ended(), 1, "the global tracer provider is still recording")
		require.Equal(t, 1, exporter.Len(), "the global logger provider is still exporting")

		_, probe := tp.Tracer("probe").Start(ctx, "probe")
		require.True(t, probe.IsRecording())
		require.True(t, lp.Logger("probe").Enabled(ctx, otellog.EnabledParameters{}))
	})
	t.Run("the fallback Otx uses the globals as well", func(t *testing.T) {
		sr.Reset()

		_, span := otx.TraceStart(t.Context(), "fallback")
		span.End()

		require.Len(t, sr.Ended(), 1)
		require.Equal(t, "fallback", sr.Ended()[0].Name())
	})
}
