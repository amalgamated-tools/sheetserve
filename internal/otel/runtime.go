package otel

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SetupRuntimeTelemetry installs logs, tracing, and metrics exporters and
// returns one shutdown function that flushes them in reverse order.
func SetupRuntimeTelemetry(ctx context.Context, shutdownTimeout time.Duration) (func(), error) {
	newctx, span := StartSpan(ctx, instrumentationName, otelkeys.OtelSetupRuntime)
	defer span.End()
	defer ObserveDuration(newctx, otelkeys.OtelSetupRuntimeDuration)()

	// installed holds one flush closure per provider that has been set up so
	// far, in installation order, so failures can unwind exactly what was
	// installed, in reverse.
	var installed []func(context.Context)

	unwind := func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		for _, i := range slices.Backward(installed) {
			i(flushCtx)
		}
	}

	providers := []struct {
		name  string
		setup func(context.Context) (func(context.Context) error, error)
	}{
		{"log export", SetupLogs},
		{"tracing", SetupTracing},
		{"metrics", SetupMetrics},
	}

	for _, p := range providers {
		shutdown, err := p.setup(ctx)
		if err != nil {
			unwind()
			return nil, fmt.Errorf("failed to set up %s: %w", p.name, err)
		}
		name := p.name
		installed = append(installed, func(flushCtx context.Context) {
			if err := shutdown(flushCtx); err != nil {
				slog.ErrorContext(flushCtx, "failed to shut down telemetry provider",
					slog.String(otelkeys.Name, name),
					slog.Any(otelkeys.Error, err))
			}
		})
	}

	return unwind, nil
}

// FlushTelemetry exports everything buffered by the global trace, log, and
// metric providers, blocking until done or ctx expires. A Render Workflows
// task process is torn down as soon as the task reports its result, before
// any deferred shutdown runs, so a task must flush before it returns or
// everything after the batch processors' first tick is lost.
func FlushTelemetry(ctx context.Context) {
	if tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		if err := tp.ForceFlush(ctx); err != nil {
			slog.WarnContext(ctx, "failed to flush traces", slog.Any(otelkeys.Error, err))
		}
	}
	if lp, ok := otel.GetLoggerProvider().(*sdklog.LoggerProvider); ok {
		if err := lp.ForceFlush(ctx); err != nil {
			slog.WarnContext(ctx, "failed to flush logs", slog.Any(otelkeys.Error, err))
		}
	}
	if mp, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); ok {
		if err := mp.ForceFlush(ctx); err != nil {
			slog.WarnContext(ctx, "failed to flush metrics", slog.Any(otelkeys.Error, err))
		}
	}
}
