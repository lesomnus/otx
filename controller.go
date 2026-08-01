package otx

import (
	"context"
	"errors"
)

// Controller is the lifecycle hook of an [Otx]. Give one with
// [WithController] to have an application's own resources - an exporter
// connection, a metric collection loop - started and stopped alongside the
// telemetry pipeline.
//
// Shutting down the providers passed to [WithTracerProvider],
// [WithMeterProvider] and [WithLoggerProvider] does not need a Controller;
// [Otx.Shutdown] already does that.
type Controller interface {
	// Start prepares the controlled resource for use. An implementation that
	// fails partway must shut down whatever it already started before
	// returning, since Shutdown is not called for a failed Start.
	Start(ctx context.Context) error

	// Shutdown releases the controlled resource. It should be safe to call
	// more than once.
	Shutdown(ctx context.Context) error
}

type noopController struct{}

func (noopController) Start(ctx context.Context) error    { return nil }
func (noopController) Shutdown(ctx context.Context) error { return nil }

// NewController adapts a pair of functions into a [Controller]. Either
// function may be nil, in which case that half is a no-op.
func NewController(start func(context.Context) error, shutdown func(context.Context) error) Controller {
	return funcController{start: start, shutdown: shutdown}
}

type funcController struct {
	start    func(context.Context) error
	shutdown func(context.Context) error
}

func (c funcController) Start(ctx context.Context) error {
	if c.start == nil {
		return nil
	}

	return c.start(ctx)
}

func (c funcController) Shutdown(ctx context.Context) error {
	if c.shutdown == nil {
		return nil
	}

	return c.shutdown(ctx)
}

// JoinControllers combines controllers into one. Nil entries are ignored.
//
// Start runs them in order; if one fails, those already started are shut down
// in reverse order and the error is returned, so that the combined Controller
// keeps the "shut down itself if it failed to start" contract. Shutdown runs
// them in reverse order and joins every error with [errors.Join].
func JoinControllers(cs ...Controller) Controller {
	vs := make([]Controller, 0, len(cs))
	for _, c := range cs {
		// isNil rather than c != nil, so that a typed nil - what a constructor
		// that returned "nil, err" hands over - is dropped here instead of
		// panicking on the first Start, the same way the options treat it.
		if !isNil(c) {
			vs = append(vs, c)
		}
	}

	return joinedController(vs)
}

type joinedController []Controller

func (cs joinedController) Start(ctx context.Context) error {
	for i, c := range cs {
		err := c.Start(ctx)
		if err == nil {
			continue
		}

		errs := []error{err}
		for j := i - 1; j >= 0; j-- {
			errs = append(errs, cs[j].Shutdown(ctx))
		}

		return errors.Join(errs...)
	}

	return nil
}

func (cs joinedController) Shutdown(ctx context.Context) error {
	errs := make([]error, 0, len(cs))
	for i := len(cs) - 1; i >= 0; i-- {
		errs = append(errs, cs[i].Shutdown(ctx))
	}

	return errors.Join(errs...)
}
