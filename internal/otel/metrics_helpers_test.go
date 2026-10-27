package otel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeterReader installs a ManualReader-backed MeterProvider as the
// global provider so instruments created earlier (package init) delegate to
// it, and returns the reader for inspection.
func newTestMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	return reader
}

// findMetric returns the metric with the given name from a collected
// ResourceMetrics, or fails the test if it isn't present.
func findMetric(t *testing.T, data metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range data.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	require.Fail(t, "metric not found in collected data", "name: %q", name)

	return metricdata.Metrics{}
}

func attrValue(attrs attribute.Set, key string) (string, bool) {
	for _, kv := range attrs.ToSlice() {
		if string(kv.Key) == key {
			return kv.Value.String(), true
		}
	}

	return "", false
}

func TestIncrAndAddRecordCounterValues(t *testing.T) {
	reader := newTestMeterReader(t)

	Incr(context.Background(), "something")
	Add(context.Background(), "something", 2)

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))

	m := findMetric(t, data, "something")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "expected an int64 counter sum")
	require.Len(t, sum.DataPoints, 1)
	require.EqualValues(t, 3, sum.DataPoints[0].Value)
}

func TestIncrWithAttributes(t *testing.T) {
	reader := newTestMeterReader(t)

	Incr(
		context.Background(), "something.with.attrs",
		attribute.String("owner", "acme"),
	)

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))

	m := findMetric(t, data, "something.with.attrs")
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "expected an int64 counter sum")
	require.Len(t, sum.DataPoints, 1)

	owner, ok := attrValue(sum.DataPoints[0].Attributes, "owner")
	require.True(t, ok)
	require.Equal(t, "acme", owner)
}

func TestRegisterInt64GaugeReportsCurrentLevel(t *testing.T) {
	reader := newTestMeterReader(t)

	scope := []string{"acme/one", "acme/two"}
	RegisterInt64Gauge("repos.in.scope", func(_ context.Context, o metric.Int64Observer) error {
		for _, r := range scope {
			o.Observe(1, metric.WithAttributes(attribute.String("repository", r)))
		}

		return nil
	})

	// A gauge is observed on collection, not on registration, so each Collect
	// re-reports the current level — collecting twice must not accumulate.
	for range 2 {
		var data metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &data))

		m := findMetric(t, data, "repos.in.scope")
		gauge, ok := m.Data.(metricdata.Gauge[int64])
		require.True(t, ok, "expected an int64 gauge")
		require.Len(t, gauge.DataPoints, 2)
		for _, dp := range gauge.DataPoints {
			require.EqualValues(t, 1, dp.Value)
		}
	}
}

func TestObserveDurationRecordsHistogram(t *testing.T) {
	reader := newTestMeterReader(t)

	done := ObserveDuration(context.Background(), "something.duration")
	time.Sleep(1 * time.Millisecond)
	done()

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))

	m := findMetric(t, data, "something.duration")
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected a float64 histogram")
	require.Len(t, hist.DataPoints, 1)
	require.EqualValues(t, 1, hist.DataPoints[0].Count)
	require.Greater(t, hist.DataPoints[0].Sum, 0.0)
}

func TestObserveDurationWithAttributes(t *testing.T) {
	reader := newTestMeterReader(t)

	done := ObserveDuration(
		context.Background(), "something.duration.attrs",
		attribute.String("owner", "acme"),
	)
	done()

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))

	m := findMetric(t, data, "something.duration.attrs")
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected a float64 histogram")
	require.Len(t, hist.DataPoints, 1)

	owner, ok := attrValue(hist.DataPoints[0].Attributes, "owner")
	require.True(t, ok)
	require.Equal(t, "acme", owner)
}
