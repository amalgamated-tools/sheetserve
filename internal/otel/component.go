package otel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

// Component values attached to log records so major runtime surfaces can be
// distinguished in shared process output.
const (
	ComponentServer = "server"
)

type componentCtxKey struct{}

// WithComponent returns a context that tags all slog *Context calls made with
// it (or contexts derived from it) with the given component attribute.
func WithComponent(ctx context.Context, component string) context.Context {
	return context.WithValue(ctx, componentCtxKey{}, component)
}

// ComponentFromContext returns the component stored by WithComponent, or ""
// if the context carries none.
func ComponentFromContext(ctx context.Context) string {
	component, _ := ctx.Value(componentCtxKey{}).(string)

	return component
}

// componentHandler wraps a slog.Handler and appends the component attribute
// from the record's context, so call sites don't have to attach it manually.
type componentHandler struct {
	inner slog.Handler
}

func newComponentHandler(inner slog.Handler) componentHandler {
	return componentHandler{inner: inner}
}

func (h componentHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h componentHandler) Handle(ctx context.Context, r slog.Record) error {
	if component := ComponentFromContext(ctx); component != "" {
		r.AddAttrs(slog.String(otelkeys.Component, component))
	}
	if fns := contextLogAttrFuncs.Load(); fns != nil {
		for _, fn := range *fns {
			if attr, ok := fn(ctx); ok {
				r.AddAttrs(attr)
			}
		}
	}

	if err := h.inner.Handle(ctx, r); err != nil {
		return fmt.Errorf("failed to handle slog record: %w", err)
	}
	return nil
}

func (h componentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return componentHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h componentHandler) WithGroup(name string) slog.Handler {
	return componentHandler{inner: h.inner.WithGroup(name)}
}
