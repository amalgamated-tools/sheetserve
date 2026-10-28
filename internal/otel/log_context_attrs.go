package otel

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel/trace"
)

// Every *Context log call made inside a span carries its trace ID, so a log
// line in the console can be found in the tracing backend.
func init() {
	RegisterContextLogAttr(func(ctx context.Context) (slog.Attr, bool) {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			return slog.String(otelkeys.TraceID, sc.TraceID().String()), true
		}

		return slog.Attr{}, false
	})
}

// ContextLogAttrFunc extracts a log attribute from a context, reporting
// false when the context carries nothing to add.
type ContextLogAttrFunc func(ctx context.Context) (slog.Attr, bool)

// contextLogAttrFuncs holds registered extractors behind an atomic pointer
// so registration never races with concurrent logging.
var contextLogAttrFuncs atomic.Pointer[[]ContextLogAttrFunc]

// RegisterContextLogAttr adds an extractor whose attribute is appended to
// every log record made with a *Context call, mirroring how the component
// attribute is attached. Packages that own a context key register here
// instead of otel importing them (e.g. middleware's request ID).
func RegisterContextLogAttr(fn ContextLogAttrFunc) {
	for {
		old := contextLogAttrFuncs.Load()
		next := make([]ContextLogAttrFunc, 0, 1)
		if old != nil {
			next = append(next, *old...)
		}
		next = append(next, fn)
		if contextLogAttrFuncs.CompareAndSwap(old, &next) {
			return
		}
	}
}
