package otxpretty

import (
	"context"
	"io"
	"strings"
	"time"

	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type LogExporter struct {
	Out io.Writer
}

func (h *LogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	for _, r := range records {
		title := ""
		kind := ""
		body := ""
		rest := make([]log.KeyValue, 0, r.AttributesLen())

		r.WalkAttributes(func(kv log.KeyValue) bool {
			switch kv.Key {
			case "internal.otx.title":
				title = kv.Value.AsString()
			case "internal.otx.ingress":
				kind = "ingress"
				for _, kv := range kv.Value.AsMap() {
					if kv.Key == "method" {
						body = kv.Value.AsString()
						break
					}
				}
			case "internal.otx.egress":
				kind = "egress"
				for _, kv := range kv.Value.AsMap() {
					if kv.Key == "elapsed" {
						dt := time.Duration(kv.Value.AsInt64()) * time.Nanosecond
						body = dt.String()
						break
					}
				}
			default:
				rest = append(rest, kv)
			}
			return true
		})

		role_c := c_faint
		if len(title) > 8 {
			title = title[:8]
		}
		if title != "" {
			sum := sum(title)
			role_c = pastel_colors[sum%len(pastel_colors)]
		}
		title = role_c.Sprintf("|%s%s| ", strings.Repeat(".", 8-len(title)), title)

		trace_id := r.TraceID()
		trace_id_c := c_faint
		if trace_id.IsValid() {
			sum := sum(trace_id.String())
			trace_id_c = dimmed_colors[sum%len(dimmed_colors)]
		}

		trace_id_s := trace_id.String()
		if len(trace_id_s) > 6 {
			trace_id_s = trace_id_s[len(trace_id_s)-6:]
		}
		trace_id_s = trace_id_c.Sprint(trace_id_s)

		span_id := r.SpanID()
		span_id_c := c_faint
		if span_id.IsValid() {
			sum := sum(span_id.String())
			span_id_c = dimmed_colors[sum%len(dimmed_colors)]
		}

		span_id_s := span_id.String()
		if len(span_id_s) > 6 {
			span_id_s = span_id_s[len(span_id_s)-6:]
		}
		span_id_s = span_id_c.Sprint(span_id_s)

		var sym string
		switch (r.Severity() - 1) / 4 {
		case 0: // Trace1~4
			sym = c_faint.Sprint(" • ")
		case 1: // Debug1~4
			sym = c_debug.Sprint(" ? ")
		case 2: // Info1~4
			sym = c_info.Sprint(" ○ ")
		case 3: // Warn1~4
			sym = c_warn.Sprint(" ! ")
		case 4: // Error1~4
			sym = c_error.Sprint(" x ")
		case 5: // Fatal1~5
			sym = c_error.Sprint("-x-")
		default:
			sym = c_error.Sprint(" • ")
		}

		var msg string
		switch kind {
		case "ingress":
			msg = c_ingress.Sprint("›» ", body)
		case "egress":
			msg = c_egress.Sprint("«‹ ", body)
		default:
			msg = c_msg.Sprint(r.Body().AsString())
		}

		b := strings.Builder{}
		b.WriteString(title)
		b.WriteString(c_time.Sprint(r.Timestamp().Format("15:04:05.000")))
		b.WriteString(sym)
		b.WriteString(trace_id_s)
		b.WriteString(" ")
		b.WriteString(span_id_s)
		b.WriteString(" ")
		b.WriteString(msg)
		b.WriteString("\n")

		// TODO: write rest

		h.Out.Write([]byte(b.String()))
	}
	return nil
}

func (h *LogExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (h *LogExporter) ForceFlush(ctx context.Context) error {
	return nil
}

func sum(s string) int {
	sum := 0
	for i := range len(s) {
		sum += int(s[i])
	}

	return sum
}
