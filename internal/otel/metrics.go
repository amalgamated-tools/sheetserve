package otel

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// SetupMetrics installs a global OpenTelemetry MeterProvider
func SetupMetrics(ctx context.Context) (func(context.Context) error, error) {
	newctx, span := StartSpan(ctx, instrumentationName, otelkeys.OtelSetupMetrics)
	defer span.End()
	defer ObserveDuration(newctx, otelkeys.OtelSetupMetricsDuration)()

	reader, err := autoexport.NewMetricReader(newctx)
	if err != nil {
		return nil, fmt.Errorf("creating metric reader: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(resourceOrPartial(newctx)),
	)
	otel.SetMeterProvider(provider)

	// Heap, GC pause, and goroutine gauges on the global meter.
	if err := otelruntime.Start(); err != nil {
		return nil, fmt.Errorf("starting runtime metrics: %w", err)
	}

	slog.DebugContext(
		newctx,
		"metrics initialized",
		slog.String(otelkeys.Exporter, exporterName("OTEL_METRICS_EXPORTER")),
	)

	Incr(
		newctx,
		otelkeys.OtelSetupMetricsSuccess,
	)
	return provider.Shutdown, nil
}
