package oteltest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// InstallSpanRecorder sets up an OpenTelemetry span recorder for testing,
// and registers cleanup to restore the previous tracer provider.
func InstallSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	t.Cleanup(func() {
		require.NoError(t, tracerProvider.Shutdown(context.Background()))
		otel.SetTracerProvider(previous)
	})

	return spanRecorder
}

// FindSpanByName searches the recorded spans for one with the given name.
// It fails the test if no span with that name is found.
func FindSpanByName(t *testing.T, recorder *tracetest.SpanRecorder, name string) tracetest.SpanStub {
	t.Helper()

	for _, ended := range recorder.Ended() {
		stub := tracetest.SpanStubFromReadOnlySpan(ended)
		if stub.Name == name {
			return stub
		}
	}
	require.FailNowf(t, "missing span", "expected span %q", name)
	return tracetest.SpanStub{}
}

// FindSpansByName returns all recorded spans with the given name.
func FindSpansByName(recorder *tracetest.SpanRecorder, name string) []tracetest.SpanStub {
	out := make([]tracetest.SpanStub, 0)
	for _, ended := range recorder.Ended() {
		stub := tracetest.SpanStubFromReadOnlySpan(ended)
		if stub.Name == name {
			out = append(out, stub)
		}
	}
	return out
}

// CountSpansByName returns the count of recorded spans with the given name.
func CountSpansByName(recorder *tracetest.SpanRecorder, name string) int {
	count := 0
	for _, ended := range recorder.Ended() {
		stub := tracetest.SpanStubFromReadOnlySpan(ended)
		if stub.Name == name {
			count++
		}
	}
	return count
}

// InstallMeterReader sets up a ManualReader-backed MeterProvider as the
// global provider for testing, and registers cleanup to shut it down. Tests
// using this should not run in parallel with each other (the global
// MeterProvider is process-wide), matching the existing convention for
// tracing/metrics tests in internal/otel.
func InstallMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		require.NoError(t, provider.Shutdown(context.Background()))
		otel.SetMeterProvider(previous)
	})

	return reader
}

// FindMetric returns the metric with the given name from a collected
// ResourceMetrics, or fails the test if it isn't present.
func FindMetric(t *testing.T, data metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	for _, sm := range data.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	require.FailNowf(t, "metric not found", "expected metric %q", name)
	return metricdata.Metrics{}
}

// AttrValue returns the string value of key within attrs, and whether it was
// present.
func AttrValue(attrs attribute.Set, key string) (string, bool) {
	for _, kv := range attrs.ToSlice() {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}
