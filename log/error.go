package log

import (
	"context"
	"log/slog"
	"reflect"

	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// ErrorTypeKey and ErrorMessageKey are the attribute keys under which
// [RecordError] records an error. ErrorTypeKey is the semantic convention
// attribute error.type; there is no convention for the message, which is why
// it is written down here once rather than spelled out at each call site.
const (
	ErrorTypeKey    = string(semconv.ErrorTypeKey)
	ErrorMessageKey = "error.message"
)

// RecordError reports err as the outcome of the operation ctx is in: it marks
// the active span as failed, records err on it as a span event, and writes an
// Error record correlated with that span. It returns err, so it reads as part
// of a return statement:
//
//	if err := do(ctx); err != nil {
//		return log.RecordError(ctx, "do the thing", err)
//	}
//
// A nil err does nothing and returns nil, so a call can be left in place on a
// path that usually succeeds.
//
// msg is the message of the record and should be a fixed string. The error
// text goes into an attribute instead, because a message built from an error
// carries hostnames, addresses and timings, and a log backend cannot group
// what it cannot repeat.
//
// It lives in this package rather than in otx because it is the only place
// that can reach both the span and the [log/slog.Logger] a caller may have
// stashed with [Into].
func RecordError(ctx context.Context, msg string, err error, attrs ...slog.Attr) error {
	if err == nil {
		return nil
	}

	text := err.Error()
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, text)
	}

	// Detached from cancellation: reporting a failure is the moment the record
	// matters most, and the context is often already cancelled by the time the
	// failure is known.
	ctx = context.WithoutCancel(ctx)
	From(ctx).LogAttrs(ctx, slog.LevelError, msg, append([]slog.Attr{
		slog.String(ErrorTypeKey, ErrorType(err)),
		slog.String(ErrorMessageKey, text),
	}, attrs...)...)

	return err
}

// ErrorType returns the value to record under [ErrorTypeKey] for err: its
// concrete Go type, which is low cardinality and groups well, unlike the
// message.
//
// The type is spelled the way the tracing SDK spells it on the exception event
// of a span - the full import path for a named type, and the plain type name
// otherwise - so that the record and the span event a single [RecordError]
// writes can be joined on it. That differs from %v of the %T verb for a type
// that is not a pointer: "github.com/me/app.myError" rather than
// "app.myError".
func ErrorType(err error) string {
	if err == nil {
		return ""
	}

	t := reflect.TypeOf(err)
	if t.PkgPath() == "" && t.Name() == "" {
		// A pointer, a slice, or another unnamed type; also the builtins.
		return t.String()
	}

	return t.PkgPath() + "." + t.Name()
}
