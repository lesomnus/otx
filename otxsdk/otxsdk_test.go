package otxsdk_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxsdk"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// deadPort is a port nothing listens on, so that a connection to it is refused
// at once rather than hanging. It is used where a test needs an export to fail.
const deadPort = "http://127.0.0.1:1"

// Every variable New or the SDK underneath it reads. The suite may run in an
// environment that already exports some of these, and a stray
// OTEL_TRACES_EXPORTER would quietly change what almost every subtest here
// builds, so each one clears the lot before setting what it means to test.
var otelEnv = []string{
	"OTEL_SDK_DISABLED",
	"OTEL_TRACES_EXPORTER",
	"OTEL_METRICS_EXPORTER",
	"OTEL_LOGS_EXPORTER",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
	"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_COMPRESSION",
	"OTEL_EXPORTER_OTLP_TIMEOUT",
	"OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE",
	"OTEL_METRIC_EXPORT_INTERVAL",
	"OTEL_METRIC_EXPORT_TIMEOUT",
	"OTEL_SERVICE_NAME",
	"OTEL_RESOURCE_ATTRIBUTES",
	"OTEL_TRACES_SAMPLER",
	"OTEL_TRACES_SAMPLER_ARG",
}

// clearEnv unsets every variable in otelEnv for the duration of the test.
//
// The variables are process global, which is why nothing in this file is
// parallel; t.Setenv enforces that.
//
// They are really unset, not set to the empty string: the two are not the same
// to the SDK. OTEL_TRACES_SAMPLER= is present but unparseable, so it reports
// "unsupported sampler" through [otel.Handle] and falls back to the default,
// which is a different starting state from the one every "nothing is set" test
// here means to describe. [testing.T.Setenv] cannot unset, so it is called
// first for the restore it registers and the guard against a parallel test,
// then the variable is removed.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, env := range otelEnv {
		t.Setenv(env, "")
		require.NoError(t, os.Unsetenv(env))
	}
}

// export is one request the collector received, reduced to what these tests
// assert on. The body is left as bytes: proving the wiring works needs the
// path, the content type and that something was actually sent, not the
// protobuf inside.
type export struct {
	method       string
	path         string
	content_type string
	body         []byte
}

// collector is an OTLP/HTTP endpoint that accepts everything.
type collector struct {
	url string

	mu      sync.Mutex
	exports []export
	// Closed and replaced every time an export arrives, so a waiter is woken
	// without polling.
	arrived chan struct{}
}

// newCollector starts a collector and points OTEL_EXPORTER_OTLP_ENDPOINT at
// it. The providers New builds then have somewhere to flush to, which is what
// makes shutting them down clean instead of a pile of refused connections.
func newCollector(t *testing.T) *collector {
	t.Helper()

	c := &collector{arrived: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		c.mu.Lock()
		c.exports = append(c.exports, export{
			method:       r.Method,
			path:         r.URL.Path,
			content_type: r.Header.Get("Content-Type"),
			body:         body,
		})
		close(c.arrived)
		c.arrived = make(chan struct{})
		c.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c.url = srv.URL
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)

	return c
}

// waitFor returns the first export the collector received on path, waiting for
// one to arrive if it has not yet. The deadline only bounds a failure; the wait
// itself is on the export, so there is nothing to sleep for.
func (c *collector) waitFor(t *testing.T, path string) export {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		c.mu.Lock()
		for _, e := range c.exports {
			if e.path == path {
				c.mu.Unlock()
				return e
			}
		}
		arrived := c.arrived
		c.mu.Unlock()

		select {
		case <-arrived:

		case <-ctx.Done():
			t.Fatalf("no export to %s: %v, got %v", path, ctx.Err(), c.paths())
			return export{}
		}
	}
}

// count returns how many exports the collector received on path.
func (c *collector) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	n := 0
	for _, e := range c.exports {
		if e.path == path {
			n++
		}
	}

	return n
}

// paths returns the path of every export received so far, in order.
func (c *collector) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	paths := make([]string, 0, len(c.exports))
	for _, e := range c.exports {
		paths = append(paths, e.path)
	}

	return paths
}

// newOtx builds through New, requiring it to succeed, and shuts the result
// down when the test ends.
func newOtx(t *testing.T, opts ...otxsdk.Option) *otx.Otx {
	t.Helper()

	x, err := otxsdk.New(t.Context(), opts...)
	require.NoError(t, err)
	require.NotNil(t, x)

	t.Cleanup(func() {
		// Not t.Context(): it is already cancelled by the time a cleanup runs,
		// and a cancelled context turns the final flush into an error.
		require.NoError(t, x.Shutdown(context.Background()))
	})

	return x
}

// recordSpans registers a recorder on the tracer provider New built, so that a
// span started through x can be read back with the resource and the scope it
// carries. The recorder is synchronous, unlike the batcher New configured.
func recordSpans(t *testing.T, x *otx.Otx) *tracetest.SpanRecorder {
	t.Helper()

	tp, ok := x.Providers().Tracer().(*sdktrace.TracerProvider)
	require.Truef(t, ok, "want an SDK tracer provider, got %T", x.Providers().Tracer())

	sr := tracetest.NewSpanRecorder()
	tp.RegisterSpanProcessor(sr)

	return sr
}

// endedSpan starts and ends one span on x and returns it as it was recorded.
func endedSpan(t *testing.T, x *otx.Otx, sr *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()

	_, span := x.TraceStart(t.Context(), "op")
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)

	return ended[0]
}

// resourceValue returns the value of key on the resource of span, and reports
// whether the resource carries it at all.
func resourceValue(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	return span.Resource().Set().Value(attribute.Key(key))
}

// requireResource asserts that the resource of span carries key with want.
func requireResource(t *testing.T, span sdktrace.ReadOnlySpan, key string, want string) {
	t.Helper()

	v, ok := resourceValue(span, key)
	require.Truef(t, ok, "resource has no %s, got %v", key, span.Resource().Attributes())
	require.Equal(t, want, v.AsString(), key)
}

func TestNewDisabled(t *testing.T) {
	t.Run("OTEL_SDK_DISABLED gives a noop provider for every signal", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_SDK_DISABLED", "true")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)
		require.NotNil(t, x)

		ps := x.Providers()
		require.IsType(t, tracenoop.NewTracerProvider(), ps.Tracer())
		require.IsType(t, metricnoop.NewMeterProvider(), ps.Meter())
		require.IsType(t, lognoop.NewLoggerProvider(), ps.Logger())

		// The instruments derived from them still work, they just record
		// nothing.
		_, span := x.TraceStart(t.Context(), "op")
		require.False(t, span.IsRecording())
		span.End()

		// Nothing was started, so nothing has to be stopped.
		require.NoError(t, x.Shutdown(t.Context()))
		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(t.Context()))
	})
	t.Run("the check happens before anything else is read", func(t *testing.T) {
		// A disabled SDK builds nothing, so a variable that would otherwise be
		// rejected is never looked at. Turning telemetry off must not be able
		// to fail.
		clearEnv(t)
		t.Setenv("OTEL_SDK_DISABLED", "TRUE")
		t.Setenv("OTEL_TRACES_EXPORTER", "jaeger")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "novalue")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)
		require.IsType(t, tracenoop.NewTracerProvider(), x.Providers().Tracer())
	})
	t.Run("otx options still apply", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_SDK_DISABLED", "true")

		x, err := otxsdk.New(t.Context(), otxsdk.WithOtxOptions(
			otx.WithPropagator(propagation.Baggage{}),
			otx.WithScopeName("app"),
		))
		require.NoError(t, err)
		require.Equal(t, []string{"baggage"}, x.Propagator().Fields())
	})
	t.Run("a value other than true builds the real providers", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_SDK_DISABLED", "false")
		newCollector(t)

		x := newOtx(t)
		require.IsType(t, &sdktrace.TracerProvider{}, x.Providers().Tracer())
	})
}

func TestNewNone(t *testing.T) {
	t.Run("an exporter of none gives a noop provider for that signal", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		ps := x.Providers()
		require.IsType(t, tracenoop.NewTracerProvider(), ps.Tracer())
		require.IsType(t, metricnoop.NewMeterProvider(), ps.Meter())
		require.IsType(t, lognoop.NewLoggerProvider(), ps.Logger())

		require.NoError(t, x.Shutdown(t.Context()))
	})
	t.Run("only the signals that ask for it are noop", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_METRICS_EXPORTER", "none")

		x := newOtx(t)

		ps := x.Providers()
		require.IsType(t, &sdktrace.TracerProvider{}, ps.Tracer())
		require.IsType(t, metricnoop.NewMeterProvider(), ps.Meter())
		require.IsType(t, &sdklog.LoggerProvider{}, ps.Logger())
	})
}

func TestNewDefault(t *testing.T) {
	t.Run("nothing set builds an SDK provider for every signal", func(t *testing.T) {
		// No collector: the OTLP/HTTP exporters connect lazily, so New has to
		// succeed against an endpoint nothing is listening on. Anything else
		// would make the start of a program depend on the collector being up
		// first.
		clearEnv(t)

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)
		require.NotNil(t, x)

		ps := x.Providers()
		require.IsType(t, &sdktrace.TracerProvider{}, ps.Tracer())
		require.IsType(t, &sdkmetric.MeterProvider{}, ps.Meter())
		require.IsType(t, &sdklog.LoggerProvider{}, ps.Logger())

		// Shutting down attempts a last export, which fails with nothing
		// listening; that it is reported and not swallowed is the point.
		_ = x.Shutdown(context.Background())
	})
	t.Run("the providers are owned, so shutting the Otx down stops them", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t)
		tp, ok := x.Providers().Tracer().(*sdktrace.TracerProvider)
		require.True(t, ok)

		require.NoError(t, x.Shutdown(t.Context()))

		// A provider the Otx did not own would still be recording here.
		_, span := tp.Tracer("after").Start(t.Context(), "op")
		require.False(t, span.IsRecording())
		span.End()
	})
	t.Run("a list builds one exporter per name", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "otlp,otlp")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x := newOtx(t)

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		// ForceFlush runs every span processor and returns once each has
		// finished, so the count below needs no waiting of its own.
		require.NoError(t, x.ForceFlush(t.Context()))

		// Two batchers, so the same span is shipped twice.
		require.Equal(t, 2, c.count("/v1/traces"))
		require.Equal(t, []string{"/v1/traces", "/v1/traces"}, c.paths())
	})
}

func TestNewConsole(t *testing.T) {
	// A provider type is the same whichever exporter is behind it, so these
	// only pin what New returns. That a console exporter really is a console
	// exporter is in TestNewConsoleOutput, which reads the stdout of a child
	// process because that is the only place the answer shows up.
	t.Run("console builds an SDK provider for every signal", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		t.Setenv("OTEL_METRICS_EXPORTER", "console")
		t.Setenv("OTEL_LOGS_EXPORTER", "console")

		x := newOtx(t)

		ps := x.Providers()
		require.IsType(t, &sdktrace.TracerProvider{}, ps.Tracer())
		require.IsType(t, &sdkmetric.MeterProvider{}, ps.Meter())
		require.IsType(t, &sdklog.LoggerProvider{}, ps.Logger())
	})
	t.Run("a console exporter never looks at the protocol", func(t *testing.T) {
		// The protocol only describes how to talk to a collector, so a value
		// that would be rejected for OTLP is irrelevant here.
		clearEnv(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "console")
		t.Setenv("OTEL_METRICS_EXPORTER", "console")
		t.Setenv("OTEL_LOGS_EXPORTER", "console")
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")

		x := newOtx(t)
		require.IsType(t, &sdktrace.TracerProvider{}, x.Providers().Tracer())
	})
}

func TestNewProtocol(t *testing.T) {
	// These are about New succeeding with nothing listening, which is what
	// lets a program start before its collector does. Which transport was
	// actually built is not visible from the provider type; that is in
	// TestNewGRPC, against a real gRPC collector.
	t.Run("grpc builds an SDK provider for every signal", func(t *testing.T) {
		// The gRPC exporters dial lazily too, so this needs no collector
		// either.
		clearEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", deadPort)
		// gRPC retries a refused connection until the export deadline, and the
		// metric reader exports once on the way down. Without a short deadline
		// that is ten seconds of backoff for nothing.
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "200")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		ps := x.Providers()
		require.IsType(t, &sdktrace.TracerProvider{}, ps.Tracer())
		require.IsType(t, &sdkmetric.MeterProvider{}, ps.Meter())
		require.IsType(t, &sdklog.LoggerProvider{}, ps.Logger())

		_ = x.Shutdown(context.Background())
	})
	t.Run("a per signal protocol builds that signal on its own transport", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", deadPort)
		t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "200")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
		t.Setenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "grpc")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)
		require.IsType(t, &sdktrace.TracerProvider{}, x.Providers().Tracer())

		_ = x.Shutdown(context.Background())
	})
}

func TestNewError(t *testing.T) {
	t.Run("a bad exporter name fails and returns no Otx", func(t *testing.T) {
		for _, env := range []string{
			"OTEL_TRACES_EXPORTER",
			"OTEL_METRICS_EXPORTER",
			"OTEL_LOGS_EXPORTER",
		} {
			t.Run(env, func(t *testing.T) {
				clearEnv(t)
				newCollector(t)
				t.Setenv(env, "jaeger")

				x, err := otxsdk.New(t.Context())
				require.Error(t, err)
				require.Nil(t, x)
				require.ErrorContains(t, err, env)
				require.ErrorContains(t, err, `"jaeger"`)
				require.ErrorContains(t, err, "otxsdk:")
			})
		}
	})
	t.Run("a bad protocol fails and returns no Otx", func(t *testing.T) {
		for _, env := range []string{
			"OTEL_EXPORTER_OTLP_PROTOCOL",
			"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
			"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
			"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
		} {
			t.Run(env, func(t *testing.T) {
				clearEnv(t)
				newCollector(t)
				t.Setenv(env, "http/json")

				x, err := otxsdk.New(t.Context())
				require.Error(t, err)
				require.Nil(t, x)
				require.ErrorContains(t, err, env)
				require.ErrorContains(t, err, `"http/json"`)
			})
		}
	})
	t.Run("the signal that failed is named", func(t *testing.T) {
		for _, tc := range []struct {
			env    string
			signal string
		}{
			{"OTEL_TRACES_EXPORTER", "otxsdk: traces:"},
			{"OTEL_METRICS_EXPORTER", "otxsdk: metrics:"},
			{"OTEL_LOGS_EXPORTER", "otxsdk: logs:"},
		} {
			t.Run(tc.env, func(t *testing.T) {
				clearEnv(t)
				newCollector(t)
				t.Setenv(tc.env, "jaeger")

				_, err := otxsdk.New(t.Context())
				require.ErrorContains(t, err, tc.signal)
			})
		}
	})
	t.Run("a cancelled context fails and returns no Otx", func(t *testing.T) {
		// The context New is given is the one the exporters are built with, so
		// a caller that has already given up gets an error rather than a set
		// of providers wired to a context that is finished with.
		clearEnv(t)
		newCollector(t)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		x, err := otxsdk.New(ctx)
		require.Error(t, err)
		require.Nil(t, x)
		require.ErrorIs(t, err, context.Canceled)

		// Traces are built first, so it fails there with nothing to unwind.
		require.ErrorContains(t, err, "otxsdk: traces:")
		require.NotContains(t, err.Error(), "while unwinding")
	})
	t.Run("a resource that cannot be built fails before any provider is", func(t *testing.T) {
		// OTEL_RESOURCE_ATTRIBUTES is read by the SDK, not by this package, and
		// an entry with no "=" makes its detector report a partial resource.
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "novalue")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)
		require.ErrorContains(t, err, "otxsdk: resource:")
		require.ErrorIs(t, err, resource.ErrPartialResource)

		// Nothing was built, so nothing was unwound.
		require.NotContains(t, err.Error(), "while unwinding")
	})
}

func TestNewUnwind(t *testing.T) {
	t.Run("a later failure shuts down the providers already built", func(t *testing.T) {
		// Traces and metrics are built first and logs fails, so the two live
		// providers have to be stopped before New returns; otherwise a failed
		// call would leave batching goroutines and a periodic reader running
		// with no owner.
		//
		// An unwind error appears because stopping a provider flushes it, and
		// with nothing listening on the endpoint that final export is refused.
		// It is the reason the implementation reports it as secondary: it is
		// noise created by the unwinding itself and never the cause, so it must
		// not be allowed to hide the bad exporter name.
		clearEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", deadPort)
		t.Setenv("OTEL_LOGS_EXPORTER", "jaeger")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)

		// The real cause, and the fact that unwinding happened.
		require.ErrorContains(t, err, "otxsdk: logs:")
		require.ErrorContains(t, err, "OTEL_LOGS_EXPORTER")
		require.ErrorContains(t, err, `"jaeger"`)
		require.ErrorContains(t, err, "otxsdk: while unwinding:")

		// The cause is joined first, so it is what a reader sees at the top.
		require.Truef(t, strings.HasPrefix(err.Error(), "otxsdk: logs:"), "got %v", err)
	})
	t.Run("the providers already built are really shut down, not merely reported as such", func(t *testing.T) {
		// The error text says unwinding happened; this says it had an effect.
		// Shutting a meter provider down makes its periodic reader collect and
		// export one last time, so a request at the collector is proof that
		// the provider built before the failure was stopped rather than
		// abandoned. Traces are off so that the only thing left to unwind is
		// the meter provider.
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
		t.Setenv("OTEL_LOGS_EXPORTER", "jaeger")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)

		// Shutdown does not return until that last export has been answered,
		// so by the time New returns the collector has it.
		require.Equal(t, []string{"/v1/metrics"}, c.paths())
	})
	t.Run("a clean unwind reports only the cause", func(t *testing.T) {
		// The same failure with a collector reachable: the providers still
		// have to be shut down, but flushing them succeeds, so there is
		// nothing secondary to report.
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_LOGS_EXPORTER", "jaeger")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)
		require.ErrorContains(t, err, "otxsdk: logs:")
		require.NotContains(t, err.Error(), "while unwinding")
	})
	t.Run("a provider with nothing to shut down is skipped", func(t *testing.T) {
		// The noop providers a signal set to none yields have no Shutdown, so
		// unwinding walks straight past them instead of failing on the type
		// assertion.
		clearEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", deadPort)
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "jaeger")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)
		require.ErrorContains(t, err, "otxsdk: logs:")
		require.NotContains(t, err.Error(), "while unwinding")
	})
	t.Run("a failure in the first signal has nothing to unwind", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", deadPort)
		t.Setenv("OTEL_TRACES_EXPORTER", "jaeger")

		x, err := otxsdk.New(t.Context())
		require.Error(t, err)
		require.Nil(t, x)
		require.ErrorContains(t, err, "otxsdk: traces:")
		require.NotContains(t, err.Error(), "while unwinding")
	})
}

func TestNewResource(t *testing.T) {
	t.Run("the resource is built from the environment", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t)
		span := endedSpan(t, x, recordSpans(t, x))

		// The SDK detector and the host detector both ran.
		requireResource(t, span, "telemetry.sdk.name", "opentelemetry")
		requireResource(t, span, "telemetry.sdk.language", "go")

		_, ok := resourceValue(span, "host.name")
		require.True(t, ok, "resource has no host.name")
	})
	t.Run("OTEL_SERVICE_NAME reaches the resource", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")

		x := newOtx(t)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "service.name", "checkout")
		requireResource(t, span, "telemetry.sdk.language", "go")
	})
	t.Run("OTEL_RESOURCE_ATTRIBUTES reaches the resource", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=staging,team=payments")

		x := newOtx(t)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "deployment.environment", "staging")
		requireResource(t, span, "team", "payments")
	})
	t.Run("without OTEL_SERVICE_NAME the resource carries the specified fallback", func(t *testing.T) {
		// The resource is merged under resource.Default, which supplies the
		// unknown_service:<binary> the specification asks for, so a program
		// that sets nothing still exports a service.name.
		clearEnv(t)
		newCollector(t)

		x := newOtx(t)
		span := endedSpan(t, x, recordSpans(t, x))

		v, ok := resourceValue(span, "service.name")
		require.True(t, ok, "got %v", span.Resource().Attributes())
		require.True(t, strings.HasPrefix(v.AsString(), "unknown_service"), "got %q", v.AsString())
	})
	t.Run("WithResource replaces the one built from the environment", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t, otxsdk.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "given"),
		)))
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "service.name", "given")

		// Replaced, not added to: none of the detectors ran.
		for _, key := range []string{"telemetry.sdk.name", "telemetry.sdk.language", "host.name"} {
			_, ok := resourceValue(span, key)
			require.Falsef(t, ok, "resource still has %s", key)
		}
	})
	t.Run("WithResource does not keep the environment out of the resource", func(t *testing.T) {
		// Documented rather than endorsed. WithResource does replace the
		// resource this package builds, which is what the test above shows,
		// but it is not the last word: every SDK provider merges
		// resource.Environment() underneath the resource it is handed, so
		// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES still reach the
		// exported resource for every key the given resource does not carry
		// itself. The test above only looks replacing because it runs with
		// those variables unset.
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "from-env")
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=payments")

		x := newOtx(t, otxsdk.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "given"),
		)))
		span := endedSpan(t, x, recordSpans(t, x))

		// The given resource wins where the two carry the same key.
		requireResource(t, span, "service.name", "given")

		// An attribute only the environment has is still there.
		requireResource(t, span, "team", "payments")

		// What WithResource does keep out is the detectors, which run in
		// newResource and nowhere else.
		_, ok := resourceValue(span, "host.name")
		require.Falsef(t, ok, "resource still has host.name, got %v", span.Resource().Attributes())
	})
	t.Run("a nil resource is ignored", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t, otxsdk.WithResource(nil))
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "telemetry.sdk.name", "opentelemetry")
	})
	t.Run("a nil resource does not clear one already given", func(t *testing.T) {
		// Ignored means ignored, not "sets it back to nothing": a helper that
		// builds a resource and returns nil on a path it does not handle must
		// not be able to undo the resource an earlier option established.
		clearEnv(t)
		newCollector(t)

		x := newOtx(t,
			otxsdk.WithResource(resource.NewSchemaless(attribute.String("service.name", "given"))),
			otxsdk.WithResource(nil),
		)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "service.name", "given")

		_, ok := resourceValue(span, "telemetry.sdk.name")
		require.False(t, ok, "the environment resource was rebuilt, got %v", span.Resource().Attributes())
	})
	t.Run("WithResourceAttributes adds to the one built from the environment", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")

		x := newOtx(t,
			otxsdk.WithResourceAttributes(attribute.String("region", "eu-west-1")),
			otxsdk.WithResourceAttributes(attribute.Int("replica", 3)),
		)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "region", "eu-west-1")
		requireResource(t, span, "service.name", "checkout")
		requireResource(t, span, "telemetry.sdk.name", "opentelemetry")

		v, ok := resourceValue(span, "replica")
		require.True(t, ok)
		require.EqualValues(t, 3, v.AsInt64())
	})
	t.Run("WithResourceAttributes is ignored when WithResource is given", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t,
			otxsdk.WithResourceAttributes(attribute.String("region", "eu-west-1")),
			otxsdk.WithResource(resource.NewSchemaless(attribute.String("service.name", "given"))),
		)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "service.name", "given")

		_, ok := resourceValue(span, "region")
		require.False(t, ok, "got %v", span.Resource().Attributes())
	})
	t.Run("the resource reaches every signal", func(t *testing.T) {
		// Neither the meter provider nor the logger provider exposes its
		// resource, so this reads the wire instead. A protobuf string is its
		// bytes, so a marker attribute can be found in the payload without
		// decoding any of it.
		clearEnv(t)
		c := newCollector(t)

		x := newOtx(t, otxsdk.WithResourceAttributes(attribute.String("region", "eu-west-1")))

		counter, err := x.Meter().Int64Counter("requests")
		require.NoError(t, err)
		counter.Add(t.Context(), 1)

		ctx, span := x.TraceStart(t.Context(), "op")
		slog.New(x.SlogHandler()).InfoContext(ctx, "hello")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))

		for _, path := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
			e := c.waitFor(t, path)
			require.Containsf(t, string(e.body), "eu-west-1", "%s carries no resource attribute", path)
		}
	})
}

func TestNewOtxOptions(t *testing.T) {
	t.Run("WithScopeName changes the scope of exported telemetry", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t, otxsdk.WithOtxOptions(
			otx.WithScopeName("app"),
			otx.WithScopeVersion("v1.2.3"),
		))
		span := endedSpan(t, x, recordSpans(t, x))

		require.Equal(t, "app", span.InstrumentationScope().Name)
		require.Equal(t, "v1.2.3", span.InstrumentationScope().Version)
	})
	t.Run("the default scope is this library", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t)
		span := endedSpan(t, x, recordSpans(t, x))

		require.Equal(t, otx.Scope, span.InstrumentationScope().Name)
	})
	t.Run("WithPropagator changes the propagator", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t, otxsdk.WithOtxOptions(otx.WithPropagator(propagation.Baggage{})))
		require.Equal(t, []string{"baggage"}, x.Propagator().Fields())
	})
	t.Run("the default propagator carries trace context and baggage", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t)
		require.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, x.Propagator().Fields())
	})
	t.Run("options from several calls all apply", func(t *testing.T) {
		// Each call adds to what is already there rather than replacing it, so
		// a caller can pass a shared set of options and add to it.
		clearEnv(t)
		newCollector(t)

		x := newOtx(t,
			otxsdk.WithOtxOptions(otx.WithScopeVersion("v9")),
			otxsdk.WithOtxOptions(otx.WithScopeName("app")),
		)
		span := endedSpan(t, x, recordSpans(t, x))

		require.Equal(t, "app", span.InstrumentationScope().Name)
		require.Equal(t, "v9", span.InstrumentationScope().Version)
	})
	t.Run("a repeated option keeps the value given last", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t,
			otxsdk.WithOtxOptions(otx.WithScopeName("first")),
			otxsdk.WithOtxOptions(otx.WithScopeName("second")),
		)
		span := endedSpan(t, x, recordSpans(t, x))

		require.Equal(t, "second", span.InstrumentationScope().Name)
	})
	t.Run("a provider given through WithOtxOptions replaces the one built here", func(t *testing.T) {
		// The options given here run after the three provider options, so a
		// provider passed through WithOtxOptions does win. That is the only way
		// to keep one signal on something else while still reading the rest of
		// the configuration from the environment.
		//
		// The traces exporter is none so that the provider being displaced is a
		// noop with nothing running behind it; displacing a real one would leave
		// its batcher with no owner to shut it down.
		clearEnv(t)
		newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "none")

		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

		x := newOtx(t, otxsdk.WithOtxOptions(otx.WithTracerProvider(tp)))
		require.Same(t, tp, x.Providers().Tracer())

		_, span := x.TraceStart(t.Context(), "op")
		span.End()
		require.Len(t, sr.Ended(), 1)
	})
	t.Run("no options is the same as none given", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t, otxsdk.WithOtxOptions())
		require.IsType(t, &sdktrace.TracerProvider{}, x.Providers().Tracer())
	})
}

func TestNewNilOption(t *testing.T) {
	t.Run("a nil Option in the list is skipped", func(t *testing.T) {
		clearEnv(t)
		newCollector(t)

		x := newOtx(t,
			nil,
			otxsdk.WithResourceAttributes(attribute.String("region", "eu-west-1")),
			nil,
		)
		span := endedSpan(t, x, recordSpans(t, x))

		requireResource(t, span, "region", "eu-west-1")
	})
	t.Run("a nil Option is skipped on the disabled path too", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OTEL_SDK_DISABLED", "true")

		x, err := otxsdk.New(t.Context(), nil)
		require.NoError(t, err)
		require.IsType(t, tracenoop.NewTracerProvider(), x.Providers().Tracer())
	})
}

func TestEndToEnd(t *testing.T) {
	t.Run("a span and a log record reach a real collector", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)
		require.NotNil(t, x)

		ctx, span := x.TraceStart(t.Context(), "op")
		slog.New(x.SlogHandler()).InfoContext(ctx, "hello")
		span.End()

		// Flush rather than wait: the batchers would ship these on their own
		// schedule, and this is what a program with a deadline does anyway.
		require.NoError(t, x.ForceFlush(t.Context()))

		traces := c.waitFor(t, "/v1/traces")
		require.Equal(t, http.MethodPost, traces.method)
		require.Equal(t, "application/x-protobuf", traces.content_type)
		require.NotEmpty(t, traces.body)

		logs := c.waitFor(t, "/v1/logs")
		require.Equal(t, http.MethodPost, logs.method)
		require.Equal(t, "application/x-protobuf", logs.content_type)
		require.NotEmpty(t, logs.body)

		// Shutting down flushes once more and stops everything, cleanly,
		// because the collector is still up.
		require.NoError(t, x.Shutdown(context.Background()))

		// Metrics are pulled by the periodic reader, which shutting down
		// triggers.
		metrics := c.waitFor(t, "/v1/metrics")
		require.Equal(t, http.MethodPost, metrics.method)
		require.NotEmpty(t, metrics.body)
	})
	t.Run("a metric recorded through the Otx reaches the collector", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		counter, err := x.Meter().Int64Counter("requests")
		require.NoError(t, err)
		counter.Add(t.Context(), 1)

		require.NoError(t, x.ForceFlush(t.Context()))

		metrics := c.waitFor(t, "/v1/metrics")
		require.Equal(t, http.MethodPost, metrics.method)
		require.Equal(t, "application/x-protobuf", metrics.content_type)
		require.NotEmpty(t, metrics.body)

		require.NoError(t, x.Shutdown(context.Background()))
	})
	t.Run("nothing is sent when every signal is none", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		_, span := x.TraceStart(t.Context(), "op")
		slog.New(x.SlogHandler()).InfoContext(t.Context(), "hello")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(context.Background()))

		require.Empty(t, c.paths())
	})
}
