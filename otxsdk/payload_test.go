package otxsdk_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/otxsdk"
	"github.com/stretchr/testify/require"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracespb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The tests in otxsdk_test.go stop at "a request arrived with a non-empty
// body". That proves the wiring but not what crossed the wire, so a provider
// built with the wrong resource, the wrong scope or a dropped record would
// still look right. These decode the payload instead.

// payloadOf returns the OTLP protobuf of e, decompressing it if the exporter
// was configured to compress. The encoding is read off the payload itself
// rather than off a header, so the collector needs to record nothing extra.
func payloadOf(t *testing.T, e export) []byte {
	t.Helper()

	if !bytes.HasPrefix(e.body, []byte{0x1f, 0x8b}) {
		return e.body
	}

	r, err := gzip.NewReader(bytes.NewReader(e.body))
	require.NoError(t, err)

	b, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())

	return b
}

func tracesOf(t *testing.T, e export) *coltracespb.ExportTraceServiceRequest {
	t.Helper()

	req := &coltracespb.ExportTraceServiceRequest{}
	require.NoError(t, proto.Unmarshal(payloadOf(t, e), req))

	return req
}

func logsOf(t *testing.T, e export) *collogspb.ExportLogsServiceRequest {
	t.Helper()

	req := &collogspb.ExportLogsServiceRequest{}
	require.NoError(t, proto.Unmarshal(payloadOf(t, e), req))

	return req
}

func metricsOf(t *testing.T, e export) *colmetricspb.ExportMetricsServiceRequest {
	t.Helper()

	req := &colmetricspb.ExportMetricsServiceRequest{}
	require.NoError(t, proto.Unmarshal(payloadOf(t, e), req))

	return req
}

// requireAttr asserts that attrs carries key with the string value want.
func requireAttr(t *testing.T, attrs []*commonpb.KeyValue, key string, want string) {
	t.Helper()

	for _, kv := range attrs {
		if kv.GetKey() == key {
			require.Equal(t, want, kv.GetValue().GetStringValue(), key)
			return
		}
	}

	t.Fatalf("no %s among %v", key, attrs)
}

// onlySpan returns the single span of req, with the resource and the scope it
// arrived under.
func onlySpan(t *testing.T, req *coltracespb.ExportTraceServiceRequest) ([]*commonpb.KeyValue, *commonpb.InstrumentationScope, *tracepb.Span) {
	t.Helper()

	require.Len(t, req.GetResourceSpans(), 1)
	rs := req.GetResourceSpans()[0]

	require.Len(t, rs.GetScopeSpans(), 1)
	ss := rs.GetScopeSpans()[0]

	require.Len(t, ss.GetSpans(), 1)

	return rs.GetResource().GetAttributes(), ss.GetScope(), ss.GetSpans()[0]
}

// onlyRecord returns the single log record of req, with its resource and
// scope.
func onlyRecord(t *testing.T, req *collogspb.ExportLogsServiceRequest) ([]*commonpb.KeyValue, *commonpb.InstrumentationScope, *logspb.LogRecord) {
	t.Helper()

	require.Len(t, req.GetResourceLogs(), 1)
	rl := req.GetResourceLogs()[0]

	require.Len(t, rl.GetScopeLogs(), 1)
	sl := rl.GetScopeLogs()[0]

	require.Len(t, sl.GetLogRecords(), 1)

	return rl.GetResource().GetAttributes(), sl.GetScope(), sl.GetLogRecords()[0]
}

// metricNamed returns the metric called name, from any scope of req.
func metricNamed(t *testing.T, req *colmetricspb.ExportMetricsServiceRequest, name string) *metricspb.Metric {
	t.Helper()

	names := []string{}
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() == name {
					return m
				}

				names = append(names, m.GetName())
			}
		}
	}

	t.Fatalf("no metric %q among %v", name, names)
	return nil
}

func TestExportedSpan(t *testing.T) {
	t.Run("the span that arrives is the one that was started", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x := newOtx(t, otxsdk.WithOtxOptions(otx.WithScopeVersion("v1.2.3")))

		_, span := x.TraceStart(t.Context(), "op")
		sc := span.SpanContext()
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))

		attrs, scope, got := onlySpan(t, tracesOf(t, c.waitFor(t, "/v1/traces")))

		require.Equal(t, "op", got.GetName())

		trace_id := sc.TraceID()
		span_id := sc.SpanID()
		require.Equal(t, trace_id[:], got.GetTraceId())
		require.Equal(t, span_id[:], got.GetSpanId())
		require.Empty(t, got.GetParentSpanId())
		require.Greater(t, got.GetEndTimeUnixNano(), uint64(0))
		require.GreaterOrEqual(t, got.GetEndTimeUnixNano(), got.GetStartTimeUnixNano())

		// The scope is the one otx was configured with, not the exporter's
		// idea of a default.
		require.Equal(t, otx.Scope, scope.GetName())
		require.Equal(t, "v1.2.3", scope.GetVersion())

		// And the resource the SDK detectors built travels with it.
		requireAttr(t, attrs, "service.name", "checkout")
		requireAttr(t, attrs, "telemetry.sdk.language", "go")
		requireAttr(t, attrs, "telemetry.sdk.name", "opentelemetry")
	})
	t.Run("a compressed export carries the same span", func(t *testing.T) {
		// OTEL_EXPORTER_OTLP_COMPRESSION is read by the exporter, not by this
		// package, so this is a check that what New builds is a fully
		// environment configured exporter and not one with the defaults nailed
		// on.
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x := newOtx(t)

		_, span := x.TraceStart(t.Context(), "op")
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))

		e := c.waitFor(t, "/v1/traces")
		require.True(t, bytes.HasPrefix(e.body, []byte{0x1f, 0x8b}), "the body is not gzipped")

		_, _, got := onlySpan(t, tracesOf(t, e))
		require.Equal(t, "op", got.GetName())
	})
}

func TestNewSampler(t *testing.T) {
	// OTEL_TRACES_SAMPLER is documented by the package comment as read by the
	// SDK. It only is read if the provider New builds is left to configure its
	// own sampler, which a WithSampler option here would silently take away.
	for _, tc := range []struct {
		what     string
		sampler  string
		arg      string
		exported bool
	}{
		{"nothing set records the span", "", "", true},
		{"always_on records the span", "always_on", "", true},
		{"always_off drops it before it reaches an exporter", "always_off", "", false},
		{"a traceidratio of 0 drops it", "traceidratio", "0", false},
		{"a traceidratio of 1 keeps it", "traceidratio", "1", true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			clearEnv(t)
			c := newCollector(t)
			t.Setenv("OTEL_METRICS_EXPORTER", "none")
			t.Setenv("OTEL_LOGS_EXPORTER", "none")
			if tc.sampler != "" {
				t.Setenv("OTEL_TRACES_SAMPLER", tc.sampler)
			}
			if tc.arg != "" {
				t.Setenv("OTEL_TRACES_SAMPLER_ARG", tc.arg)
			}

			x := newOtx(t)

			_, span := x.TraceStart(t.Context(), "op")
			span.End()

			require.Equal(t, tc.exported, span.SpanContext().IsSampled())

			// ForceFlush returns once every processor has finished, so an
			// empty collector afterwards means nothing was ever going to be
			// sent rather than that it has not been sent yet.
			require.NoError(t, x.ForceFlush(t.Context()))
			if !tc.exported {
				require.Empty(t, c.paths())
				return
			}

			_, _, got := onlySpan(t, tracesOf(t, c.waitFor(t, "/v1/traces")))
			require.Equal(t, "op", got.GetName())
		})
	}
}

func TestExportedLogRecord(t *testing.T) {
	t.Run("the record that arrives is the one that was logged, in the span it was logged in", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")

		x := newOtx(t)

		ctx, span := x.TraceStart(t.Context(), "op")
		sc := span.SpanContext()
		slog.New(x.SlogHandler()).WarnContext(ctx, "hello", slog.String("who", "world"))
		span.End()

		require.NoError(t, x.ForceFlush(t.Context()))

		attrs, scope, got := onlyRecord(t, logsOf(t, c.waitFor(t, "/v1/logs")))

		require.Equal(t, "hello", got.GetBody().GetStringValue())
		require.Equal(t, "WARN", got.GetSeverityText())
		require.Equal(t, logspb.SeverityNumber_SEVERITY_NUMBER_WARN, got.GetSeverityNumber())
		requireAttr(t, got.GetAttributes(), "who", "world")

		// The record was written inside the span, so it is correlated with it;
		// this is the whole point of carrying the providers together.
		trace_id := sc.TraceID()
		span_id := sc.SpanID()
		require.Equal(t, trace_id[:], got.GetTraceId())
		require.Equal(t, span_id[:], got.GetSpanId())

		require.Equal(t, otx.Scope, scope.GetName())
		requireAttr(t, attrs, "service.name", "checkout")
	})
}

func TestExportedMetric(t *testing.T) {
	t.Run("the metric that arrives is the one that was recorded", func(t *testing.T) {
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_SERVICE_NAME", "checkout")
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x := newOtx(t)

		counter, err := x.Meter().Int64Counter("requests")
		require.NoError(t, err)
		counter.Add(t.Context(), 2)

		require.NoError(t, x.ForceFlush(t.Context()))

		req := metricsOf(t, c.waitFor(t, "/v1/metrics"))
		require.Len(t, req.GetResourceMetrics(), 1)
		requireAttr(t, req.GetResourceMetrics()[0].GetResource().GetAttributes(), "service.name", "checkout")
		require.Equal(t, otx.Scope, req.GetResourceMetrics()[0].GetScopeMetrics()[0].GetScope().GetName())

		m := metricNamed(t, req, "requests")
		require.Len(t, m.GetSum().GetDataPoints(), 1)
		require.Equal(t, int64(2), m.GetSum().GetDataPoints()[0].GetAsInt())
		require.True(t, m.GetSum().GetIsMonotonic())
		require.Equal(t,
			metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			m.GetSum().GetAggregationTemporality(),
		)
	})
	t.Run("the temporality asked for by the environment is the one exported", func(t *testing.T) {
		// Another variable this package never touches, on a code path it does:
		// the reader New builds is configured from the environment like any
		// other, so asking for delta changes what the collector receives.
		clearEnv(t)
		c := newCollector(t)
		t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")

		x, err := otxsdk.New(t.Context())
		require.NoError(t, err)

		counter, err := x.Meter().Int64Counter("requests")
		require.NoError(t, err)
		counter.Add(t.Context(), 1)

		require.NoError(t, x.ForceFlush(t.Context()))
		require.NoError(t, x.Shutdown(context.Background()))

		m := metricNamed(t, metricsOf(t, c.waitFor(t, "/v1/metrics")), "requests")
		require.Equal(t,
			metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
			m.GetSum().GetAggregationTemporality(),
		)
	})
}
