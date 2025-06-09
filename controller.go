package otx

import "context"

type Controller interface {
	// Shutdown itself if failed to start.
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

type noopController struct{}

func (noopController) Start(ctx context.Context) error    { return nil }
func (noopController) Shutdown(ctx context.Context) error { return nil }
