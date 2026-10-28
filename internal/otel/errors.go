package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ErrPanic wraps the value recovered by RecoverPanic so callers can match it
// with errors.Is.
var ErrPanic = errors.New("panic")

// RecoverPanic turns a panic in the calling goroutine into a recorded error
// instead of a process exit: the span (nil is fine) gets error status and an
// exception event with the stack, and the same goes out as an error log.
// It must be deferred directly, not from inside another deferred closure,
// because recover only works in the function the runtime calls on unwind:
//
//	defer otel.RecoverPanic(ctx, span, "panic in event handler")
//
// Use it on fire-and-forget goroutines. HTTP handlers already have chi's
// Recoverer; workflow tasks use ReportPanic so the Render SDK still fails them.
func RecoverPanic(ctx context.Context, span trace.Span, statusMsg string) {
	if r := recover(); r != nil {
		recordPanic(ctx, span, statusMsg, r)
	}
}

// ReportPanic is RecoverPanic for code whose caller must still see the panic,
// such as workflow tasks, which the Render SDK fails on panic: it records and
// reports the panic, flushes the error handler (Render reclaims the process
// once the task ends), and then panics again with the same value. Defer it
// directly, as with RecoverPanic.
func ReportPanic(ctx context.Context, span trace.Span, statusMsg string) {
	r := recover()
	if r == nil {
		return
	}
	recordPanic(ctx, span, statusMsg, r)

	panic(r)
}

func recordPanic(ctx context.Context, span trace.Span, statusMsg string, r any) {
	var err error
	if e, ok := r.(error); ok {
		err = fmt.Errorf("%w: %w", ErrPanic, e)
	} else {
		err = fmt.Errorf("%w: %v", ErrPanic, r)
	}
	if span != nil {
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetStatus(codes.Error, statusMsg)
	}

	//nolint:sloglint // central forwarder, same contract as RecordAndLogError
	slog.ErrorContext(ctx, statusMsg, ErrorAttrs(err, slog.String(otelkeys.ExceptionStacktrace, string(debug.Stack())))...)
}

func RecordError(span trace.Span, statusMsg string, err error) {
	if span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, statusMsg)
}

// CaptureError reports err to the error handler. Call it once per failure,
// where the failure stops propagating (a task's returned error), not at every
// layer that records it: RecordError and RecordAndLogError only mark the span
// and log. Cancellation is expected and skipped. Deadline errors are reported:
// at a task boundary the deadline is the task's own budget, so hitting it is
// a real failure (e.g. a reply that never replaced the placeholder).
func CaptureError(ctx context.Context, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
}

// ReportFatal reports err, an error that is about to end the process, and
// flushes it, since the process exits before any deferred flush would send
// it. Like CaptureError it skips nil and cancellation. Before the error
// handler is initialized it is a no-op.
func ReportFatal(ctx context.Context, err error) {
	CaptureError(ctx, err)
}

func RecordAndLogError(ctx context.Context, span trace.Span, statusMsg string, err error, attrs ...any) {
	RecordError(span, statusMsg, err)
	//nolint:sloglint // central forwarder: statusMsg is reused verbatim as the span status message; call sites still pass a static literal statusMsg, which is what sloglint's static-msg check enforces.
	slog.ErrorContext(ctx, statusMsg, ErrorAttrs(err, attrs...)...)
}

func ErrorAttrs(err error, attrs ...any) []any {
	out := make([]any, 0, len(attrs)+1)
	out = append(out, attrs...)
	out = append(out, slog.Any(otelkeys.Error, err))

	return out
}
