package otx

import "context"

type Controller interface {
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

type noopController struct{}

func (noopController) Start(ctx context.Context) error    { return nil }
func (noopController) Shutdown(ctx context.Context) error { return nil }
