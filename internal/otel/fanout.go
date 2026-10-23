package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// fanoutHandler duplicates each record to every wrapped handler, letting the
// process write human-readable console output and export OTLP logs from the
// same slog calls. Each handler applies its own Enabled filtering, so the
// console and the exporter can sit at different levels.
type fanoutHandler struct {
	handlers []slog.Handler
}

func newFanoutHandler(handlers ...slog.Handler) fanoutHandler {
	return fanoutHandler{handlers: handlers}
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}

	return false
}

func (h fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	// Find the last enabled handler so it can take the original record. Every
	// earlier handler gets a clone: a handler that appends attrs (via
	// AddAttrs) would otherwise mutate the shared record backing and leak
	// attributes into the sibling handlers that follow it. The last one has no
	// siblings left to protect, so cloning for it is a wasted allocation per
	// line on the logging hot path.
	last := -1
	for i, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			last = i
		}
	}

	var errs []error
	for i, handler := range h.handlers {
		if !handler.Enabled(ctx, r.Level) {
			continue
		}
		rec := r
		if i != last {
			rec = r.Clone()
		}
		if err := handler.Handle(ctx, rec); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}

	return fanoutHandler{handlers: handlers}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}

	return fanoutHandler{handlers: handlers}
}

// minLevelHandler imposes a minimum level on a handler that doesn't filter by
// itself. The otelslog bridge delegates Enabled to the backend logger, which
// accepts everything — without this, a LOG_LEVEL=info process would still
// export debug records.
type minLevelHandler struct {
	inner slog.Handler
	level slog.Leveler
}

func (h minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level.Level() && h.inner.Enabled(ctx, level)
}

func (h minLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.inner.Handle(ctx, r); err != nil {
		return fmt.Errorf("failed to handle slog record: %w", err)
	}
	return nil
}

func (h minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevelHandler{inner: h.inner.WithAttrs(attrs), level: h.level}
}

func (h minLevelHandler) WithGroup(name string) slog.Handler {
	return minLevelHandler{inner: h.inner.WithGroup(name), level: h.level}
}
