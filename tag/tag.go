package tag

import (
	"log/slog"

	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const TitleKey = string(semconv.AppWidgetNameKey)

func Title(title string) slog.Attr {
	a := slog.String(TitleKey, title)
	return a
}
