package tag

import (
	"log/slog"
	"time"
)

const TitleKey = "app.widget.name"

func Title(title string) slog.Attr {
	a := slog.String(TitleKey, title)
	return a
}

const IngressKey = "internal.otx.ingress"

func Ingress(method string) slog.Attr {
	a := slog.Group(IngressKey, slog.String("method", method))
	return a
}

const EgressKey = "internal.otx.egress"

func Egress(elapsed time.Duration) slog.Attr {
	a := slog.Group(EgressKey, slog.Duration("elapsed", elapsed))
	return a
}
