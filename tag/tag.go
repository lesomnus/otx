// Package tag holds constructors for the log attributes this repository uses
// by convention, so that the key of each one is written down exactly once.
package tag

import (
	"log/slog"

	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// TitleKey is the key under which [Title] records its value. It is the
// semantic convention attribute app.widget.name, borrowed as the label of a
// unit of work.
const TitleKey = string(semconv.AppWidgetNameKey)

// Title returns the attribute naming the unit of work a record belongs to.
func Title(title string) slog.Attr {
	return slog.String(TitleKey, title)
}
