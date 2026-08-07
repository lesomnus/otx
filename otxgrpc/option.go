package otxgrpc

import "google.golang.org/grpc/stats"

// Filter reports whether an RPC is one to write records for. A filter must
// return true for an RPC that is.
//
// It is the same shape as
// [go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc.Filter],
// so a predicate written for the tracing handler serves here without being
// rewritten.
type Filter func(*stats.RPCTagInfo) bool

// Option configures a logger returned by [NewServerLogger] or
// [NewClientLogger].
type Option interface {
	apply(*rpcLogger)
}

type optionFunc func(*rpcLogger)

func (f optionFunc) apply(l *rpcLogger) { f(l) }

// WithFilter writes records only for the RPCs f returns true for.
//
// It is for the RPCs that are polled rather than called -- a health check every
// few seconds, from every replica, says nothing that reading it will ever
// repay, and it arrives often enough to be most of what is kept.
//
// What is filtered is the records this handler writes, and nothing else. A
// filtered RPC still carries the [github.com/lesomnus/otx.Otx] and the service
// and method attributes in its context, so a handler that logs on its own is
// unaffected, and the tracing handler decides for itself.
//
// A nil f is ignored rather than taken as a filter that accepts nothing, so a
// caller that works one out need not check first.
func WithFilter(f Filter) Option {
	return optionFunc(func(l *rpcLogger) {
		if f == nil {
			return
		}

		l.filter = f
	})
}
