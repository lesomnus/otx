package otxsdk_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"

	"github.com/lesomnus/otx/otxsdk"
	"github.com/stretchr/testify/require"
)

// childEnv marks the process started by runChild, which is this test binary
// run again.
//
// A console exporter writes to os.Stdout, and both stdouttrace and stdoutlog
// read os.Stdout once, into a package level variable, when their package is
// initialised - long before any test could swap it for a pipe. The only way
// left to see what a console exporter produced is to be a different process
// and own its stdout, which is what this does.
const childEnv = "OTXSDK_TEST_CHILD"

// TestChildProcess is the body runChild executes. It is a no-op unless this
// process is that child, so a normal run reports it as skipped.
//
// It builds through [otxsdk.New] and emits one of each signal, so what the
// parent reads on stdout is whatever the environment it passed selected.
func TestChildProcess(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		t.Skip("runs only as the child process of runChild")
	}

	x, err := otxsdk.New(t.Context())
	require.NoError(t, err)

	counter, err := x.Meter().Int64Counter("requests")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)

	ctx, span := x.TraceStart(t.Context(), "op")
	slog.New(x.SlogHandler()).InfoContext(ctx, "hello")
	span.End()

	require.NoError(t, x.ForceFlush(t.Context()))
	require.NoError(t, x.Shutdown(context.Background()))
}

// runChild runs [TestChildProcess] in a child process and returns everything
// it wrote to stdout.
//
// The child inherits the environment as the caller has already set it up, so
// it is configured exactly like the process under test. Waiting for it to exit
// is the synchronisation: by then it has flushed and shut down.
func runChild(t *testing.T) string {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$")
	cmd.Env = append(os.Environ(), childEnv+"=1")

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	out, err := cmd.Output()
	require.NoErrorf(t, err, "child: %s\nstdout: %s\nstderr: %s", err, out, stderr)

	return string(out)
}

func TestNewConsoleOutput(t *testing.T) {
	t.Run("a console exporter writes to stdout instead of to the collector", func(t *testing.T) {
		// The provider being an SDK one says nothing about which exporter is
		// behind it: OTLP yields exactly the same type. What tells the two
		// apart is where the telemetry ends up, so both halves are asserted -
		// stdout has all three signals, and the collector, which is up and
		// named by OTEL_EXPORTER_OTLP_ENDPOINT, never saw any of them.
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		t.Setenv("OTEL_METRICS_EXPORTER", "console")
		t.Setenv("OTEL_LOGS_EXPORTER", "console")
		t.Setenv("OTEL_SERVICE_NAME", "checkout")

		out := runChild(t)
		require.Contains(t, out, `"Name":"op"`)
		require.Contains(t, out, `"Body":{"Type":"String","Value":"hello"}`)
		require.Contains(t, out, `"Name":"requests"`)

		// What it prints is the whole record, resource and scope included, so
		// a console exporter is configured like any other and not handed a
		// bare span.
		require.Contains(t, out, `"Value":"checkout"`)
		require.Contains(t, out, `"telemetry.sdk.language"`)
		require.Contains(t, out, `"Name":"github.com/lesomnus/otx"`)

		require.Empty(t, c.paths())
	})
	t.Run("a list of otlp and console feeds both", func(t *testing.T) {
		// Every name in the list gets its own exporter, so the one span is
		// printed and shipped. Only traces is a list; the other two signals are
		// off, so the text on stdout can only have come from the traces
		// exporter and the request can only be the span.
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp,console")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		out := runChild(t)
		require.Contains(t, out, `"Name":"op"`)

		require.Equal(t, []string{"/v1/traces"}, c.paths())
	})
	t.Run("nothing is printed when every signal is otlp", func(t *testing.T) {
		// The mirror of the first case: the same child, the same span, and
		// nothing on stdout, so the assertions above are about the console
		// exporter and not about anything the child prints on its own.
		clearEnv(t)
		c := newCollector(t)

		out := runChild(t)
		require.NotContains(t, out, `"Name":"op"`)
		require.NotContains(t, out, "hello")

		require.Equal(t, 1, c.count("/v1/traces"))
		require.Equal(t, 1, c.count("/v1/logs"))
		// The periodic reader collects on the way down as well as on the
		// flush, and a cumulative counter has something to report both times.
		require.GreaterOrEqual(t, c.count("/v1/metrics"), 1)
	})
}
