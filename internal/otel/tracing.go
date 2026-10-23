package otel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/version"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationName = "github.com/amalgamated-tools/sheetserve/internal/otel"

const defaultServiceName = "sheetserve"

// wrapDefaultTransport guards the http.DefaultTransport wrap so repeated
// SetupTracing calls (tests) do not nest transports and double-report spans.
var wrapDefaultTransport sync.Once

// SetupTracing initializes OpenTelemetry tracing with a global TracerProvider.
func SetupTracing(ctx context.Context) (func(context.Context) error, error) {
	newctx, span := StartSpan(ctx, instrumentationName, otelkeys.OtelSetupTracing)
	defer span.End()
	defer ObserveDuration(newctx, otelkeys.OtelSetupTracingDuration)()

	exporter, err := autoexport.NewSpanExporter(newctx)
	if err != nil {
		return nil, fmt.Errorf("creating span exporter: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resourceOrPartial(newctx)),
	}

	provider := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(provider)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Every outbound client in this process effectively leaves Transport nil,
	// so wrapping the default transport once gives each request a client span
	// and a traceparent header without threading an http.Client through five
	// constructors.
	wrapDefaultTransport.Do(func() {
		http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)
	})

	slog.DebugContext(
		newctx,
		"tracing initialized",
		slog.String(otelkeys.Exporter, exporterName("OTEL_TRACES_EXPORTER")),
	)

	return provider.Shutdown, nil
}

func newResource(ctx context.Context) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(defaultServiceName),
		semconv.ServiceVersion(version.String()),
	}
	if env := os.Getenv(environmentVar); env != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(strings.ToLower(env)))
	}
	r, err := resource.New(
		ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	return r, nil
}

func resourceOrPartial(ctx context.Context) *resource.Resource {
	res, err := newResource(ctx)
	if err != nil {
		slog.WarnContext(ctx, "partial OTEL resource", slog.Any(otelkeys.Error, err))
	}

	return res
}

func exporterName(envVar string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}

	return "otlp"
}

// StartSpan creates a span using the specific package name provided by the caller
func StartSpan(ctx context.Context, instrName string, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	// Dynamically grabs the tracer for the calling package's identity
	tracer := otel.GetTracerProvider().Tracer(instrName)

	return tracer.Start(ctx, spanName, opts...) //nolint:spancheck // span is ended by the caller, not this function
}
