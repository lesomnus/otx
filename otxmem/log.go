package otxmem

import (
	"context"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type LogExporter struct {
	Records []sdklog.Record
}

func (h *LogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if h.Records == nil {
		h.Records = []sdklog.Record{}
	}

	h.Records = append(h.Records, records...)
	return nil
}

func (h *LogExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (h *LogExporter) ForceFlush(ctx context.Context) error {
	return nil
}
