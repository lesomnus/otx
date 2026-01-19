package log

import "log/slog"

func WidgetName(name string) slog.Attr {
	return slog.String("app.widget.name", name)
}
