package log_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxmem"
	"github.com/lesomnus/otx/otxtest"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// exceptionEventName and the exception attribute keys are what
// [go.opentelemetry.io/otel/trace.Span.RecordError] writes; they are spelled
// out rather than imported from semconv so that a semconv bump that renames
// them shows up as a failure here instead of silently changing what the tests
// assert.
const (
	exceptionEventName  = "exception"
	exceptionTypeKey    = "exception.type"
	exceptionMessageKey = "exception.message"
)

// errorStringType is the concrete type of an error from [errors.New]. Both
// halves of RecordError report a type, and this is the one nearly every caller
// hands it.
const errorStringType = "*errors.errorString"

// ptrError is an error whose concrete type is a pointer, the usual shape.
type ptrError struct {
	msg string
}

func (e *ptrError) Error() string {
	return e.msg
}

// valError is an error whose concrete type is not a pointer. It exists because
// the SDK derives the type of a span exception from the package path while
// [log.ErrorType] uses %T, and the two only differ for a named non-pointer
// type.
type valError struct {
	msg string
}

func (e valError) Error() string {
	return e.msg
}

// nilSafeError is an error whose Error method survives a nil receiver, so that
// a typed nil can be carried all the way through RecordError. A typed nil
// [ptrError] cannot: its Error method dereferences.
type nilSafeError struct {
	msg string
}

func (e *nilSafeError) Error() string {
	if e == nil {
		return "<nil>"
	}

	return e.msg
}

// strError is an error whose concrete type is neither a pointer nor a struct.
type strError string

func (e strError) Error() string {
	return string(e)
}

// countingError numbers each read of its text, so that a test can tell how
// many times RecordError asks an error for its message and which of the three
// places it writes got which read.
//
// The counter is a pointer so that the error can be passed by value, which is
// how a caller would pass one.
type countingError struct {
	n *int
}

func (e countingError) Error() string {
	*e.n++
	return strconv.Itoa(*e.n)
}

// spanProbe records what RecordError asks of the span it finds in the context,
// so that "nothing was set" can be told from "set on a span that ignores it".
type spanProbe struct {
	tracenoop.Span

	is_recording bool
	errors       []error
	statuses     []spanStatus
}

type spanStatus struct {
	code codes.Code
	desc string
}

func (s *spanProbe) IsRecording() bool {
	return s.is_recording
}

func (s *spanProbe) RecordError(err error, opts ...trace.EventOption) {
	s.errors = append(s.errors, err)
}

func (s *spanProbe) SetStatus(code codes.Code, description string) {
	s.statuses = append(s.statuses, spanStatus{code: code, desc: description})
}

var _ trace.Span = (*spanProbe)(nil)

// exportedKeys returns the keys of the attributes of an exported record, in
// order, so that "the attributes of the caller come after the error ones" can
// be asserted rather than only "they are all there".
func exportedKeys(record sdklog.Record) []string {
	keys := []string{}
	record.WalkAttributes(func(kv otellog.KeyValue) bool {
		keys = append(keys, kv.Key)
		return true
	})

	return keys
}

// requireOnlyRecord asserts that exactly one record was exported and returns
// it. Every test that says anything about the record goes through it so that
// "recorded once" cannot be mistaken for "recorded twice, the last one right".
func requireOnlyRecord(t *testing.T, h *otxtest.Harness) sdklog.Record {
	t.Helper()

	records := h.Records()
	require.Len(t, records, 1, "expected exactly one record")

	return records[0]
}

// requireOnlyEnded is [requireOnlyRecord] for spans.
func requireOnlyEnded(t *testing.T, h *otxtest.Harness) sdktrace.ReadOnlySpan {
	t.Helper()

	ended := h.Ended()
	require.Len(t, ended, 1, "expected exactly one ended span")

	return ended[0]
}

// exceptionEvent returns the only event of an ended span named "exception".
func exceptionEvent(t *testing.T, span sdktrace.ReadOnlySpan) sdktrace.Event {
	t.Helper()

	events := []sdktrace.Event{}
	for _, event := range span.Events() {
		if event.Name == exceptionEventName {
			events = append(events, event)
		}
	}
	require.Len(t, events, 1, "expected exactly one %q event but the span has %v", exceptionEventName, eventNames(span))

	return events[0]
}

// eventNames returns the names of the events of an ended span, in order.
func eventNames(span sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(span.Events()))
	for _, event := range span.Events() {
		names = append(names, event.Name)
	}

	return names
}

// eventAttrs returns the attributes of a span event by key.
func eventAttrs(event sdktrace.Event) map[string]string {
	attrs := map[string]string{}
	for _, kv := range event.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}

	return attrs
}

// exceptionMessages returns the message of every exception event of an ended
// span, sorted, so that a concurrent test can compare sets rather than orders.
func exceptionMessages(span sdktrace.ReadOnlySpan) []string {
	msgs := []string{}
	for _, event := range span.Events() {
		if event.Name != exceptionEventName {
			continue
		}

		msgs = append(msgs, eventAttrs(event)[exceptionMessageKey])
	}
	slices.Sort(msgs)

	return msgs
}

// recordedMessages returns the error message attribute of every exported
// record, sorted, alongside [exceptionMessages].
func recordedMessages(records []sdklog.Record) []string {
	msgs := make([]string, 0, len(records))
	for _, record := range records {
		msgs = append(msgs, exportedAttrs(record)[log.ErrorMessageKey].AsString())
	}
	slices.Sort(msgs)

	return msgs
}

// otelErrors collects what the SDK hands to [otel.Handle], which is where a
// record that could not be exported ends up.
type otelErrors struct {
	mu   sync.Mutex
	errs []error
	off  bool
}

func (h *otelErrors) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.off {
		return
	}

	h.errs = append(h.errs, err)
}

func (h *otelErrors) Errors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]error(nil), h.errs...)
}

// captureOtelErrors installs a recorder as the OpenTelemetry error handler for
// the duration of the test and restores the previous one afterwards.
//
// The handler is process global, and the first one a process installs stays
// wired to the default delegator for good, so the recorder is switched off as
// well as uninstalled: an error handled by a later test must not land in a
// slice nothing reads any more. Nothing in this package runs in parallel.
func captureOtelErrors(t *testing.T) *otelErrors {
	t.Helper()

	prev := otel.GetErrorHandler()
	h := &otelErrors{}
	otel.SetErrorHandler(h)

	t.Cleanup(func() {
		otel.SetErrorHandler(prev)

		h.mu.Lock()
		defer h.mu.Unlock()
		h.off = true
	})

	return h
}

func TestErrorKeys(t *testing.T) {
	t.Run("the error is recorded under the documented keys", func(t *testing.T) {
		// A backend groups on these strings, so they are the contract; the
		// type one is the semantic convention and the message one is not.
		require.Equal(t, "error.type", log.ErrorTypeKey)
		require.Equal(t, "error.message", log.ErrorMessageKey)
	})
}

func TestRecordError(t *testing.T) {
	t.Run("the active span is marked failed with the error text", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", errors.New("boom"))
		span.End()

		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Error, ended.Status().Code)
		require.Equal(t, "boom", ended.Status().Description, "the status carries the message, not the error")
	})
	t.Run("the error is recorded as an exception event on the span", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", errors.New("boom"))
		span.End()

		ended := requireOnlyEnded(t, h)
		require.Equal(t, []string{exceptionEventName}, eventNames(ended))
		require.Equal(t, map[string]string{
			exceptionTypeKey:    errorStringType,
			exceptionMessageKey: "boom",
		}, eventAttrs(exceptionEvent(t, ended)))
	})
	t.Run("the exception event carries the type of a custom error", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", &ptrError{msg: "boom"})
		span.End()

		attrs := eventAttrs(exceptionEvent(t, requireOnlyEnded(t, h)))
		require.Equal(t, "*log_test.ptrError", attrs[exceptionTypeKey])
		require.Equal(t, "boom", attrs[exceptionMessageKey])
	})
	t.Run("the type on the span event and the one on the record agree", func(t *testing.T) {
		// The two halves of one RecordError have to be joinable on the type,
		// so ErrorType spells a named type the way the tracing SDK spells it
		// on the exception event: with its full import path. A value error is
		// the case where a %T-derived type would disagree.
		for _, tc := range []struct {
			desc string
			err  error
		}{
			{desc: "a value error", err: valError{msg: "boom"}},
			{desc: "a pointer error", err: &ptrError{msg: "boom"}},
			{desc: "an error that is not a struct", err: strError("boom")},
			{desc: "a plain error", err: errors.New("boom")},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h := otxtest.New(t)
				ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

				log.RecordError(ctx, "do the thing", tc.err)
				span.End()

				on_event := eventAttrs(exceptionEvent(t, requireOnlyEnded(t, h)))[exceptionTypeKey]
				on_record := exportedAttrs(requireOnlyRecord(t, h))[log.ErrorTypeKey].AsString()
				require.Equal(t, on_event, on_record)
				require.Equal(t, log.ErrorType(tc.err), on_record)
			})
		}
	})
	t.Run("the span is given the error value itself", func(t *testing.T) {
		probe := &spanProbe{is_recording: true}
		err := &ptrError{msg: "boom"}

		ctx := trace.ContextWithSpan(t.Context(), probe)
		log.RecordError(ctx, "do the thing", err)

		require.Len(t, probe.errors, 1)
		require.Same(t, err, probe.errors[0])
		require.Equal(t, []spanStatus{{code: codes.Error, desc: "boom"}}, probe.statuses)
	})
	t.Run("the body of the record is the message and not the error", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errors.New("boom"))

		record := requireOnlyRecord(t, h)
		require.Equal(t, "do the thing", record.Body().AsString())
		require.NotContains(t, record.Body().AsString(), "boom", "the error text leaked into the message")
	})
	t.Run("the record is at the error level", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errors.New("boom"))

		record := requireOnlyRecord(t, h)
		require.Equal(t, otellog.SeverityError, record.Severity())
		require.Equal(t, slog.LevelError.String(), record.SeverityText())
	})
	t.Run("the record carries the type and the message of the error", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errors.New("boom"))

		attrs := exportedAttrs(requireOnlyRecord(t, h))
		require.Equal(t, errorStringType, attrs[log.ErrorTypeKey].AsString())
		require.Equal(t, "boom", attrs[log.ErrorMessageKey].AsString())
	})
	t.Run("the type on the record is the one ErrorType reports", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		err := &ptrError{msg: "boom"}
		log.RecordError(ctx, "do the thing", err)

		attrs := exportedAttrs(requireOnlyRecord(t, h))
		require.Equal(t, log.ErrorType(err), attrs[log.ErrorTypeKey].AsString())
		require.Equal(t, "*log_test.ptrError", attrs[log.ErrorTypeKey].AsString())
	})
	t.Run("the attributes of the caller come after the error attributes", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errors.New("boom"),
			slog.String("k", "v"),
			slog.Int("n", 1),
		)

		record := requireOnlyRecord(t, h)
		require.Equal(t, []string{
			log.ErrorTypeKey,
			log.ErrorMessageKey,
			"k",
			"n",
			log.TraceIDKey,
			log.SpanIDKey,
		}, exportedKeys(record), "an attribute was dropped or written out of order")

		attrs := exportedAttrs(record)
		require.Equal(t, "v", attrs["k"].AsString())
		require.Equal(t, int64(1), attrs["n"].AsInt64())
	})
	t.Run("the record is correlated with the active span", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errors.New("boom"))

		record := requireOnlyRecord(t, h)

		// Filled in by the SDK from the context handed to Emit, so they are
		// only there if the context of the call really reached it.
		require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
		require.Equal(t, span.SpanContext().SpanID(), record.SpanID())

		attrs := exportedAttrs(record)
		require.Equal(t, traceIDOf(span), attrs[log.TraceIDKey].AsString())
		require.Equal(t, spanIDOf(span), attrs[log.SpanIDKey].AsString())
	})
	t.Run("the error it was given is the error it returns", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		err := &ptrError{msg: "boom"}
		require.Same(t, err, log.RecordError(ctx, "do the thing", err))
	})
	t.Run("a wrapped error is returned unwrapped", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		err := fmt.Errorf("wrapped: %w", errBoom)
		actual := log.RecordError(ctx, "do the thing", err)
		require.ErrorIs(t, actual, errBoom)

		// Same, not Equal: the contract is that the very value handed over
		// comes back, which a copy carrying the same text would also satisfy.
		require.Same(t, err, actual)
	})
	t.Run("a nil error records nothing and returns nil", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		require.NoError(t, log.RecordError(ctx, "do the thing", nil))
		span.End()

		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Unset, ended.Status().Code, "the span was marked failed without an error")
		require.Empty(t, ended.Status().Description)
		require.Empty(t, ended.Events())
		require.Empty(t, h.Records(), "a record was written for a nil error")
	})
	t.Run("a nil error leaves a span that is already failed alone", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		span.SetStatus(codes.Error, "boom")
		require.NoError(t, log.RecordError(ctx, "do the thing", nil))
		span.End()

		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Error, ended.Status().Code)
		require.Equal(t, "boom", ended.Status().Description)
	})
	t.Run("a context without a span still writes a record", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		var err error
		require.NotPanics(t, func() { err = log.RecordError(ctx, "do the thing", errBoom) })
		require.ErrorIs(t, err, errBoom)

		record := requireOnlyRecord(t, h)
		require.Equal(t, "do the thing", record.Body().AsString())
		require.Equal(t, []string{log.ErrorTypeKey, log.ErrorMessageKey}, exportedKeys(record), "the record carries span attributes without a span")
		require.False(t, record.TraceID().IsValid())
		require.False(t, record.SpanID().IsValid())
		require.Empty(t, h.Ended(), "a span was ended without one being started")
	})
	t.Run("nothing is asked of a span that is not recording", func(t *testing.T) {
		probe := &spanProbe{is_recording: false}

		h := otxtest.New(t)
		ctx := trace.ContextWithSpan(h.Into(t.Context()), probe)
		log.RecordError(ctx, "do the thing", errBoom)

		require.Empty(t, probe.errors, "the error was recorded on a span that is not recording")
		require.Empty(t, probe.statuses, "the status was set on a span that is not recording")

		record := requireOnlyRecord(t, h)
		require.Equal(t, "do the thing", record.Body().AsString())
	})
	t.Run("a span that is sampled out is left alone but the record is written", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		tracer_provider := sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.NeverSample()),
			sdktrace.WithSpanProcessor(spans),
		)

		h := otxtest.New(t, otx.WithTracerProvider(tracer_provider))
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		require.False(t, span.IsRecording())
		require.True(t, span.SpanContext().IsValid(), "the span has no ids to correlate the record with")

		log.RecordError(ctx, "do the thing", errBoom)
		span.End()

		require.Empty(t, spans.Ended(), "a span that is not recording reached the processor")

		// The record is written either way: a sampling decision is about how
		// much trace detail to keep, not about whether an error happened.
		record := requireOnlyRecord(t, h)
		require.Equal(t, "do the thing", record.Body().AsString())
		require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
		require.Equal(t, span.SpanContext().SpanID(), record.SpanID())

		attrs := exportedAttrs(record)
		require.Equal(t, traceIDOf(span), attrs[log.TraceIDKey].AsString())
		require.Equal(t, spanIDOf(span), attrs[log.SpanIDKey].AsString())
	})
	t.Run("the record goes to a logger stashed with Into", func(t *testing.T) {
		// The reason RecordError lives in this package: it has to reach the
		// stashed logger, which only this package knows about.
		r := newRecorder()
		ctx := log.Into(t.Context(), slog.New(r))

		log.RecordError(ctx, "do the thing", errBoom, slog.String("k", "v"))

		c := requireOnlyHandle(t, r)
		// Not the caller's context itself: RecordError detaches it from
		// cancellation before writing. Everything the context carries is
		// still there, which is what the handler is given it for.
		require.NotNil(t, c.ctx)
		require.Nil(t, c.ctx.Done(), "the context handed over is still cancellable")
		require.Equal(t, slog.LevelError, c.level)
		require.Equal(t, "do the thing", c.record.Message)
		require.Equal(t, []string{
			log.ErrorTypeKey + "=" + errorStringType,
			log.ErrorMessageKey + "=boom",
			"k=v",
		}, attrsOf(c.record))
	})
	t.Run("a logger stashed with Into wins over the Otx", func(t *testing.T) {
		r := newRecorder()

		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		ctx = log.Into(ctx, slog.New(r))

		log.RecordError(ctx, "do the thing", errBoom)
		span.End()

		// The handler is given a context derived from this one, detached from
		// cancellation, still carrying the span it was started under.
		got := requireOnlyHandle(t, r).ctx
		require.Equal(t, span.SpanContext(), trace.SpanContextFromContext(got))
		require.Empty(t, h.Records(), "the record went to the logger of the Otx as well")

		// The span half does not care where the record went.
		require.Equal(t, codes.Error, requireOnlyEnded(t, h).Status().Code)
	})
	t.Run("the attributes and the ids reach the stashed logger together", func(t *testing.T) {
		r := newRecorder()

		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()
		ctx = log.Into(ctx, slog.New(r).With(slog.String("a", "b")))

		log.RecordError(ctx, "do the thing", errBoom, slog.Int("n", 1))

		c := requireOnlyHandle(t, r)
		require.Equal(t, []string{"a=b"}, namesOf(c.attrs))
		require.Equal(t, []string{
			log.ErrorTypeKey + "=" + errorStringType,
			log.ErrorMessageKey + "=boom",
			"n=1",
			log.TraceIDKey + "=" + traceIDOf(span),
			log.SpanIDKey + "=" + spanIDOf(span),
		}, attrsOf(c.record))
	})
	t.Run("a context that carries nothing does not panic", func(t *testing.T) {
		ctx := mark(t.Context(), "empty")
		_, ok := otx.FromOK(ctx)
		require.False(t, ok)

		require.NotPanics(t, func() {
			require.ErrorIs(t, log.RecordError(ctx, "do the thing", errBoom), errBoom)
			require.NoError(t, log.RecordError(ctx, "do the thing", nil))
		})
	})
	t.Run("each call writes its own record and its own event", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "first", errors.New("one"))
		log.RecordError(ctx, "second", errors.New("two"))
		span.End()

		records := h.Records()
		require.Len(t, records, 2)
		require.Equal(t, "first", records[0].Body().AsString())
		require.Equal(t, "second", records[1].Body().AsString())

		ended := requireOnlyEnded(t, h)
		require.Equal(t, []string{exceptionEventName, exceptionEventName}, eventNames(ended))

		// The status of a span is a single field, so the last error wins.
		require.Equal(t, "two", ended.Status().Description)
	})
	t.Run("a typed nil error is reported like any other error", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		// The guard is an interface nil check, and a nil *nilSafeError held in
		// an error is not a nil error, so nothing is skipped: a typed nil is
		// recorded, and the caller gets a non-nil error back from a call it
		// probably expected to be a no-op.
		var err *nilSafeError
		actual := log.RecordError(ctx, "do the thing", error(err))
		span.End()

		require.Error(t, actual)
		require.Equal(t, "<nil>", actual.Error())

		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Error, ended.Status().Code)
		require.Equal(t, map[string]string{
			exceptionTypeKey:    "*log_test.nilSafeError",
			exceptionMessageKey: "<nil>",
		}, eventAttrs(exceptionEvent(t, ended)))

		attrs := exportedAttrs(requireOnlyRecord(t, h))
		require.Equal(t, "*log_test.nilSafeError", attrs[log.ErrorTypeKey].AsString())
		require.Equal(t, "<nil>", attrs[log.ErrorMessageKey].AsString())
	})
	t.Run("a typed nil error whose message dereferences panics", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		// Actual behaviour, written down rather than endorsed. The nil guard
		// does not protect a typed nil, so the error is asked for its message
		// like any other and the panic is the caller's own Error method.
		var err *ptrError
		require.PanicsWithError(t, "runtime error: invalid memory address or nil pointer dereference", func() {
			log.RecordError(ctx, "do the thing", error(err))
		})

		// The panic happens inside the span half, before anything is written.
		require.Empty(t, h.Records(), "a record was written before the panic")
	})
	t.Run("a typed nil error whose message dereferences panics without a span too", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		// The other half of the same hole: with no span to record on, the
		// message is still read, this time while building the record.
		require.False(t, trace.SpanFromContext(ctx).IsRecording(), "the span half is what panicked")

		var err *ptrError
		require.PanicsWithError(t, "runtime error: invalid memory address or nil pointer dereference", func() {
			log.RecordError(ctx, "do the thing", error(err))
		})
		require.Empty(t, h.Records())
	})
	t.Run("the message of the error is read once per place it is written", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		n := 0
		log.RecordError(ctx, "do the thing", countingError{n: &n})
		span.End()

		// Read twice: once by RecordError, which reuses the one text for the
		// status of the span and for the record, and once by the SDK, which
		// derives the exception event from the error value itself and cannot
		// be handed a string. So the status and the record always agree, even
		// for an error whose text is not stable.
		require.Equal(t, 2, n)

		ended := requireOnlyEnded(t, h)
		require.Equal(t, "1", exportedAttrs(requireOnlyRecord(t, h))[log.ErrorMessageKey].AsString())
		require.Equal(t, "1", ended.Status().Description)
		require.Equal(t, "2", eventAttrs(exceptionEvent(t, ended))[exceptionMessageKey])
	})
	t.Run("the message of the error is read even when the record is dropped", func(t *testing.T) {
		r := newRecorder()
		r.disabled = true

		probe := &spanProbe{is_recording: false}
		ctx := trace.ContextWithSpan(log.Into(t.Context(), slog.New(r)), probe)

		n := 0
		err := error(countingError{n: &n})
		require.Equal(t, err, log.RecordError(ctx, "do the thing", err))

		// The attributes of the record are built before the handler is asked
		// whether it wants it, so a disabled handler saves the write but not
		// the cost of the message.
		require.Len(t, r.EnabledCalls(), 1)
		require.Empty(t, r.HandleCalls(), "a record reached a handler that reported it is not enabled")
		require.Equal(t, 1, n, "the message was read a different number of times")
	})
	t.Run("the record survives a context that is already cancelled", func(t *testing.T) {
		handled := captureOtelErrors(t)

		h := otxtest.New(t)
		ctx, cancel := context.WithCancel(h.Into(t.Context()))
		ctx, span := otx.TraceStart(ctx, "op")
		cancel()

		// The shape of nearly every real call: the error being reported is the
		// reason the context is done.
		require.ErrorIs(t, log.RecordError(ctx, "do the thing", ctx.Err()), context.Canceled)
		span.End()

		// The span half is unaffected: it does not touch the context.
		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Error, ended.Status().Code)
		require.Equal(t, "context canceled", ended.Status().Description)

		// Reporting a failure is the moment the record matters most, and by
		// then the context is usually already done - here the error being
		// reported is the cancellation itself. RecordError detaches from
		// cancellation so that an exporter which honours the context, this one
		// and every network exporter, still takes it.
		record := requireOnlyRecord(t, h)
		require.Equal(t, "do the thing", record.Body().AsString())
		require.Equal(t, "context canceled", exportedAttrs(record)[log.ErrorMessageKey].AsString())
		require.Empty(t, handled.Errors(), "nothing should have been dropped")
	})
	t.Run("a cancelled context still reaches a logger stashed with Into", func(t *testing.T) {
		r := newRecorder()
		ctx, cancel := context.WithCancel(log.Into(t.Context(), slog.New(r)))
		cancel()

		// The other side of the case above: a plain handler takes the record
		// too, and is handed a context that is no longer cancellable.
		require.ErrorIs(t, log.RecordError(ctx, "do the thing", ctx.Err()), context.Canceled)

		c := requireOnlyHandle(t, r)
		require.Nil(t, c.ctx.Done(), "the context handed over is still cancellable")
		require.Equal(t, "do the thing", c.record.Message)
		require.Equal(t, []string{
			log.ErrorTypeKey + "=*errors.errorString",
			log.ErrorMessageKey + "=context canceled",
		}, attrsOf(c.record))
	})
	t.Run("the record reaches an exporter behind a batch processor when it is flushed", func(t *testing.T) {
		exporter := &otxmem.LogExporter{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(
			exporter,
			// Long enough that nothing is exported until ForceFlush asks, so
			// that "not yet" below is a fact rather than a race.
			sdklog.WithExportInterval(time.Hour),
		)))

		// The option replaces the logger provider of the Harness, which then
		// owns this one and shuts it down when the test ends.
		h := otxtest.New(t, otx.WithLoggerProvider(provider))
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		defer span.End()

		log.RecordError(ctx, "do the thing", errBoom)
		require.Zero(t, exporter.Len(), "a batch processor exported before it was asked to flush")

		require.NoError(t, provider.ForceFlush(t.Context()))

		records := exporter.Records()
		require.Len(t, records, 1)
		require.Equal(t, "do the thing", records[0].Body().AsString())
		require.Equal(t, span.SpanContext().SpanID(), records[0].SpanID())

		attrs := exportedAttrs(records[0])
		require.Equal(t, errorStringType, attrs[log.ErrorTypeKey].AsString())
		require.Equal(t, "boom", attrs[log.ErrorMessageKey].AsString())
	})
	t.Run("concurrent calls on one span each write their own record and event", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		const n = 8

		expected := make([]string, 0, n)
		for i := range n {
			expected = append(expected, fmt.Sprintf("boom %d", i))
		}
		slices.Sort(expected)

		wg := sync.WaitGroup{}
		wg.Add(n)
		for i := range n {
			go func() {
				defer wg.Done()
				log.RecordError(ctx, "do the thing", fmt.Errorf("boom %d", i))
			}()
		}
		wg.Wait()
		span.End()

		// Nothing is lost and nothing is written twice, in either half.
		require.Equal(t, expected, exceptionMessages(requireOnlyEnded(t, h)))
		require.Equal(t, expected, recordedMessages(h.Records()))
	})
	t.Run("no metric is recorded", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", errBoom)
		span.End()

		// The call did reach the two signals it is about, so an empty third
		// one is a decision and not an inert test.
		require.NotEmpty(t, h.Records())
		require.NotEmpty(t, h.Ended())

		// Counting errors is the job of the caller, which knows what the
		// operation was; a counter here would be one dimension of nothing.
		require.Empty(t, h.Collect(t.Context()).ScopeMetrics, "a metric was recorded")
	})
	t.Run("a remote parent is left alone but the record is correlated with it", func(t *testing.T) {
		h := otxtest.New(t)

		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
			SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		ctx := trace.ContextWithRemoteSpanContext(h.Into(t.Context()), sc)

		// Valid and sampled, but not a span of this process: there is nothing
		// here to record an event on.
		remote := trace.SpanFromContext(ctx)
		require.True(t, remote.SpanContext().IsValid())
		require.True(t, remote.SpanContext().IsRemote())
		require.False(t, remote.IsRecording())

		log.RecordError(ctx, "do the thing", errBoom)

		require.Empty(t, h.Ended(), "a span was ended without one being started")

		record := requireOnlyRecord(t, h)
		require.Equal(t, sc.TraceID(), record.TraceID())
		require.Equal(t, sc.SpanID(), record.SpanID())

		attrs := exportedAttrs(record)
		require.Equal(t, sc.TraceID().String(), attrs[log.TraceIDKey].AsString())
		require.Equal(t, sc.SpanID().String(), attrs[log.SpanIDKey].AsString())
	})
	t.Run("an exception attribute is dropped when the span limits them", func(t *testing.T) {
		limits := sdktrace.NewSpanLimits()
		limits.AttributePerEventCountLimit = 1

		spans := tracetest.NewSpanRecorder()
		tracer_provider := sdktrace.NewTracerProvider(
			sdktrace.WithSpanLimits(limits),
			sdktrace.WithSpanProcessor(spans),
		)

		h := otxtest.New(t, otx.WithTracerProvider(tracer_provider))
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", errBoom)
		span.End()

		// Actual behaviour: the limit is applied to the exception event like
		// to any other, so the message of the error can be lost from the span
		// while it stays on the record. The exact assertions on the event
		// elsewhere in this file hold only because the default limits are
		// generous.
		require.Len(t, spans.Ended(), 1)
		event := exceptionEvent(t, spans.Ended()[0])
		require.Equal(t, map[string]string{exceptionTypeKey: errorStringType}, eventAttrs(event))
		require.Equal(t, 1, event.DroppedAttributeCount)

		require.Equal(t, "boom", exportedAttrs(requireOnlyRecord(t, h))[log.ErrorMessageKey].AsString())
	})
	t.Run("the attributes of the caller do not reach the span", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "do the thing", errBoom, slog.String("k", "v"))
		span.End()

		// Documented asymmetry: the extra attributes go on the record only.
		// The span carries the exception it was given and nothing else.
		ended := requireOnlyEnded(t, h)
		require.Equal(t, map[string]string{
			exceptionTypeKey:    errorStringType,
			exceptionMessageKey: "boom",
		}, eventAttrs(exceptionEvent(t, ended)))
		require.Empty(t, ended.Attributes())

		require.Contains(t, exportedAttrs(requireOnlyRecord(t, h)), "k", "the attribute reached neither half")
	})
	t.Run("an attribute of the caller replaces the error attribute it collides with", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		log.RecordError(ctx, "do the thing", errBoom, slog.String(log.ErrorTypeKey, "mine"))

		// Actual behaviour: the log SDK deduplicates the attributes of a
		// record and the last one wins, so an attribute of the caller silently
		// replaces the type of the error rather than being dropped or kept
		// beside it.
		record := requireOnlyRecord(t, h)
		require.Equal(t, []string{log.ErrorTypeKey, log.ErrorMessageKey}, exportedKeys(record))
		require.Equal(t, []string{"mine"}, exportedValues(record, log.ErrorTypeKey))
	})
	t.Run("a colliding attribute is kept beside the error one by a plain handler", func(t *testing.T) {
		r := newRecorder()
		ctx := log.Into(t.Context(), slog.New(r))

		log.RecordError(ctx, "do the thing", errBoom, slog.String(log.ErrorTypeKey, "mine"))

		// The other side of the case above: a [log/slog] handler is handed
		// both, and what happens to them is its own business.
		require.Equal(t, []string{errorStringType, "mine"}, valuesOf(requireOnlyHandle(t, r).record, log.ErrorTypeKey))
	})
	t.Run("a group of attributes reaches the record as a group", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		log.RecordError(ctx, "do the thing", errBoom, slog.Group("g", slog.String("k", "v")))

		record := requireOnlyRecord(t, h)
		require.Equal(t, []string{log.ErrorTypeKey, log.ErrorMessageKey, "g"}, exportedKeys(record))

		g := exportedAttrs(record)["g"]
		require.Equal(t, otellog.KindMap, g.Kind(), "the group was flattened")
		require.Equal(t, []otellog.KeyValue{otellog.String("k", "v")}, g.AsMap())
	})
	t.Run("a span that has already ended is left alone but the record is written", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")
		span.End()

		log.RecordError(ctx, "do the thing", errBoom)

		// An ended span stops recording, so a late report cannot corrupt it.
		ended := requireOnlyEnded(t, h)
		require.Empty(t, ended.Events(), "an event was added to a span that had already ended")
		require.Equal(t, codes.Unset, ended.Status().Code)

		// Its ids are still in the context, so the record is still correlated.
		record := requireOnlyRecord(t, h)
		require.Equal(t, span.SpanContext().TraceID(), record.TraceID())
		require.Equal(t, span.SpanContext().SpanID(), record.SpanID())
	})
	t.Run("an empty message and an empty error are recorded as they are", func(t *testing.T) {
		h := otxtest.New(t)
		ctx, span := otx.TraceStart(h.Into(t.Context()), "op")

		log.RecordError(ctx, "", errors.New(""))
		span.End()

		// Nothing is substituted for an empty string, and nothing is dropped
		// for being empty: an empty error is still a failure.
		record := requireOnlyRecord(t, h)
		require.Empty(t, record.Body().AsString())
		require.Equal(t, []string{
			log.ErrorTypeKey,
			log.ErrorMessageKey,
			log.TraceIDKey,
			log.SpanIDKey,
		}, exportedKeys(record), "an attribute was dropped for being empty")
		require.Empty(t, exportedAttrs(record)[log.ErrorMessageKey].AsString())

		ended := requireOnlyEnded(t, h)
		require.Equal(t, codes.Error, ended.Status().Code, "an empty message left the span unmarked")
		require.Empty(t, ended.Status().Description)
		require.Equal(t, map[string]string{
			exceptionTypeKey:    errorStringType,
			exceptionMessageKey: "",
		}, eventAttrs(exceptionEvent(t, ended)))
	})
}

func TestErrorType(t *testing.T) {
	t.Run("a nil error has no type", func(t *testing.T) {
		require.Equal(t, "", log.ErrorType(nil))
	})
	t.Run("the concrete type of a plain error", func(t *testing.T) {
		require.Equal(t, errorStringType, log.ErrorType(errors.New("boom")))
	})
	t.Run("the concrete type of a custom error", func(t *testing.T) {
		// A named type is spelled with its full import path, the way the
		// tracing SDK spells it on the exception event of a span, so that the
		// record and the event can be joined on the type. A pointer type has
		// no name of its own and keeps the short form.
		require.Equal(t, "*log_test.ptrError", log.ErrorType(&ptrError{msg: "boom"}))
		require.Equal(t, "github.com/lesomnus/otx/log_test.valError", log.ErrorType(valError{msg: "boom"}))
	})
	t.Run("the concrete type of an error that is not a struct", func(t *testing.T) {
		// The type has to be named to group at all; an unnamed one would put
		// its shape in the attribute.
		require.Equal(t, "github.com/lesomnus/otx/log_test.strError", log.ErrorType(strError("boom")))
	})
	t.Run("the concrete type of an error from another package", func(t *testing.T) {
		require.Equal(t, "*fs.PathError", log.ErrorType(&fs.PathError{Op: "open", Path: "/nope", Err: fs.ErrNotExist}))
		// A named type from another package keeps its full path too. The local
		// valError above is the same shape.
		require.Equal(t, "syscall.Errno", log.ErrorType(syscall.ENOENT))
	})
	t.Run("a wrapped error reports the type of the wrapper", func(t *testing.T) {
		err := fmt.Errorf("wrapped: %w", &ptrError{msg: "boom"})

		// Documented behaviour: the type is the one of the value handed over,
		// not the one of the cause, so that a message built by a caller and
		// the type it is recorded under always describe the same value.
		require.Equal(t, "*fmt.wrapError", log.ErrorType(err))
		require.NotEqual(t, log.ErrorType(errors.Unwrap(err)), log.ErrorType(err))
	})
	t.Run("an error wrapping two errors reports the type of the wrapper", func(t *testing.T) {
		err := fmt.Errorf("%w and %w", errors.New("one"), errors.New("two"))
		require.Equal(t, "*fmt.wrapErrors", log.ErrorType(err))
	})
	t.Run("a joined error reports the type of the join", func(t *testing.T) {
		err := errors.Join(errors.New("one"), errors.New("two"))
		require.Equal(t, "*errors.joinError", log.ErrorType(err))
	})
	t.Run("a typed nil pointer has the type it is typed with", func(t *testing.T) {
		// Not the nil interface, so it is not the nil case: the type is there
		// even though the value is nil. Recording it is what a caller gets for
		// returning a typed nil, which is a mistake at the call site.
		var err *ptrError
		require.Equal(t, "*log_test.ptrError", log.ErrorType(error(err)))
	})
	t.Run("the value on the record is the one of the error the caller passed", func(t *testing.T) {
		h := otxtest.New(t)
		ctx := h.Into(t.Context())

		for _, tc := range []struct {
			desc string
			err  error
		}{
			{desc: "a plain error", err: errors.New("boom")},
			{desc: "a pointer error", err: &ptrError{msg: "boom"}},
			{desc: "a value error", err: valError{msg: "boom"}},
			{desc: "a wrapped error", err: fmt.Errorf("wrapped: %w", errBoom)},
		} {
			t.Run(tc.desc, func(t *testing.T) {
				h.Reset()
				log.RecordError(ctx, "do the thing", tc.err)

				attrs := exportedAttrs(requireOnlyRecord(t, h))
				require.Equal(t, log.ErrorType(tc.err), attrs[log.ErrorTypeKey].AsString())
				require.Equal(t, tc.err.Error(), attrs[log.ErrorMessageKey].AsString())
			})
		}
	})
}

// errBoom is the error of every subtest that is not about the type of one.
var errBoom = errors.New("boom")
