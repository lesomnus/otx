package otxhttp

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/tag"
)

func BoundaryLogger() func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := time.Now()
			l := log.From(r.Context())

			l.Info("in", tag.Ingress(r.RequestURI))

			w_ := &httpResWriter{ResponseWriter: w, code: 0}
			h.ServeHTTP(w_, r)

			dt := time.Since(t)
			level := slog.LevelInfo
			attrs := []any{tag.Egress(dt)}
			if w_.code >= 400 {
				level = slog.LevelWarn

				attrs = append(attrs, slog.Int("code", w_.code))
			}

			ctx := context.WithoutCancel(r.Context())
			l.Log(ctx, level, "out", attrs...)
		})
	}
}

type httpResWriter struct {
	http.ResponseWriter
	code int
}

func (w *httpResWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
