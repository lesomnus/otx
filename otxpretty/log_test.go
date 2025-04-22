package otxpretty_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/lesomnus/otx/otxpretty"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestPrettyLogExporter(t *testing.T) {
	timestamp, err := time.Parse(time.RFC3339, "1985-10-26T01:20:34.567Z")
	require.NoError(t, err)

	t.Run("title is right aligned", func(t *testing.T) {
		b := &bytes.Buffer{}
		b.WriteString("\n")
		e := &otxpretty.LogExporter{Out: b}
		p := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(e)))
		l := p.Logger("test")

		r := log.Record{}
		r.SetBody(log.StringValue("foo"))
		r.SetTimestamp(timestamp)
		r.AddAttributes(log.String("internal.otx.title", "bar"))
		l.Emit(t.Context(), r)

		expected := `
|.....bar| 01:20:34.567 • 000000 000000 foo
`
		require.Equal(t, expected, b.String())
	})
	t.Run("long title is cropped", func(t *testing.T) {
		b := &bytes.Buffer{}
		b.WriteString("\n")
		e := &otxpretty.LogExporter{Out: b}
		p := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(e)))
		l := p.Logger("test")

		r := log.Record{}
		r.SetBody(log.StringValue("foo"))
		r.SetTimestamp(timestamp)
		r.AddAttributes(log.String("internal.otx.title", "foobarbaz"))
		l.Emit(t.Context(), r)

		expected := `
|foobarba| 01:20:34.567 • 000000 000000 foo
`
		require.Equal(t, expected, b.String())
	})
	t.Run("severities", func(t *testing.T) {
		b := &bytes.Buffer{}
		b.WriteString("\n")
		e := &otxpretty.LogExporter{Out: b}
		p := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(e)))
		l := p.Logger("test")

		r := log.Record{}
		r.SetBody(log.StringValue("foo"))
		r.SetTimestamp(timestamp)

		r.SetSeverity(log.SeverityTrace)
		l.Emit(t.Context(), r)
		r.SetSeverity(log.SeverityDebug)
		l.Emit(t.Context(), r)
		r.SetSeverity(log.SeverityInfo)
		l.Emit(t.Context(), r)
		r.SetSeverity(log.SeverityWarn)
		l.Emit(t.Context(), r)
		r.SetSeverity(log.SeverityError)
		l.Emit(t.Context(), r)
		r.SetSeverity(log.SeverityFatal)
		l.Emit(t.Context(), r)

		expected := `
|........| 01:20:34.567 • 000000 000000 foo
|........| 01:20:34.567 ? 000000 000000 foo
|........| 01:20:34.567 ○ 000000 000000 foo
|........| 01:20:34.567 ! 000000 000000 foo
|........| 01:20:34.567 x 000000 000000 foo
|........| 01:20:34.567-x-000000 000000 foo
`
		require.Equal(t, expected, b.String())
	})
	t.Run("network io", func(t *testing.T) {
		b := &bytes.Buffer{}
		b.WriteString("\n")
		e := &otxpretty.LogExporter{Out: b}
		p := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(e)))
		l := p.Logger("test")

		{
			r := log.Record{}
			r.SetBody(log.StringValue("foo"))
			r.SetTimestamp(timestamp)
			r.AddAttributes(log.Map("internal.otx.ingress",
				log.String("method", "bar.baz"),
			))
			l.Emit(t.Context(), r)
		}
		{
			r := log.Record{}
			r.SetBody(log.StringValue("foo"))
			r.SetTimestamp(timestamp)
			r.AddAttributes(log.Map("internal.otx.egress",
				log.Int64("elapsed", int64(83*time.Second)),
			))
			l.Emit(t.Context(), r)
		}

		expected := `
|........| 01:20:34.567 • 000000 000000 ›» bar.baz
|........| 01:20:34.567 • 000000 000000 «‹ 1m23s
`
		require.Equal(t, expected, b.String())
	})
	t.Run("span", func(t *testing.T) {
		b := &bytes.Buffer{}
		b.WriteString("\n")

		trace_exporter := sdktrace.NewTracerProvider(sdktrace.WithIDGenerator(dumbIdGenerator{}))
		tracer := trace_exporter.Tracer("test")
		ctx, _ := tracer.Start(t.Context(), "some name")

		e := &otxpretty.LogExporter{Out: b}
		p := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(e)))
		l := p.Logger("test")

		{
			r := log.Record{}
			r.SetBody(log.StringValue("foo"))
			r.SetTimestamp(timestamp)
			r.AddAttributes(log.Map("internal.otx.ingress",
				log.String("method", "bar.baz"),
			))
			l.Emit(ctx, r)
		}
		{
			r := log.Record{}
			r.SetBody(log.StringValue("foo"))
			r.SetTimestamp(timestamp)
			l.Emit(ctx, r)
		}
		{
			r := log.Record{}
			r.SetBody(log.StringValue("foo"))
			r.SetTimestamp(timestamp)
			r.AddAttributes(log.Map("internal.otx.egress",
				log.Int64("elapsed", int64(83*time.Second)),
			))
			l.Emit(ctx, r)
		}

		expected := `
|........| 01:20:34.567 • 1a1a1a 2b2b2b ›» bar.baz
|........| 01:20:34.567 • 1a1a1a 2b2b2b foo
|........| 01:20:34.567 • 1a1a1a 2b2b2b «‹ 1m23s
`
		require.Equal(t, expected, b.String())
	})
}

type dumbIdGenerator struct{}

func (g dumbIdGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	return [16]byte{
			0x1A, 0x1A, 0x1A, 0x1A,
			0x1A, 0x1A, 0x1A, 0x1A,
			0x1A, 0x1A, 0x1A, 0x1A,
			0x1A, 0x1A, 0x1A, 0x1A,
		}, [8]byte{
			0x2B, 0x2B, 0x2B, 0x2B,
			0x2B, 0x2B, 0x2B, 0x2B,
		}
}

func (g dumbIdGenerator) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	return [8]byte{
		0x3C, 0x3C, 0x3C, 0x3C,
		0x3C, 0x3C, 0x3C, 0x3C,
	}
}
