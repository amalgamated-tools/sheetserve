package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestMeterCacheReusesHandles verifies the hot path resolves an instrument
// once and hands back the same handle on subsequent calls for an unchanged
// provider — the whole point of caching the handle.
func TestMeterCacheReusesHandles(t *testing.T) {
	newTestMeterReader(t)

	c1 := ActiveMeterCache()
	h1, err := c1.Histogram("reuse.duration")
	require.NoError(t, err)

	c2 := ActiveMeterCache()
	require.Same(t, c1, c2, "cache should not be rebuilt while the provider is unchanged")

	h2, err := c2.Histogram("reuse.duration")
	require.NoError(t, err)
	require.Equal(t, h1, h2, "same provider + name should return the same handle")
}

// TestMeterCacheRebuildsOnProviderSwap is the correctness guard that lets us
// cache at all: when the global MeterProvider is replaced, the cache must
// re-resolve so recordings land on the new provider, not the retired one.
func TestMeterCacheRebuildsOnProviderSwap(t *testing.T) {
	reader1 := newTestMeterReader(t)
	first := ActiveMeterCache()
	Incr(context.Background(), "swap.counter")

	reader2 := newTestMeterReader(t) // installs a second provider
	second := ActiveMeterCache()
	require.NotSame(t, first, second, "provider swap should rebuild the cache")
	Incr(context.Background(), "swap.counter")

	// The first provider saw exactly the one increment recorded before the swap.
	var data1 metricdata.ResourceMetrics
	require.NoError(t, reader1.Collect(context.Background(), &data1))
	sum1, ok := findMetric(t, data1, "swap.counter").Data.(metricdata.Sum[int64])
	require.True(t, ok, "expected metric data to be of type metricdata.Sum[int64]")
	require.EqualValues(t, 1, sum1.DataPoints[0].Value)

	// The second provider saw the post-swap increment — proving the cached
	// handle did not keep reporting to the retired provider.
	var data2 metricdata.ResourceMetrics
	require.NoError(t, reader2.Collect(context.Background(), &data2))
	sum2, ok := findMetric(t, data2, "swap.counter").Data.(metricdata.Sum[int64])
	require.True(t, ok, "expected metric data to be of type metricdata.Sum[int64]")
	require.EqualValues(t, 1, sum2.DataPoints[0].Value)
}

// TestMeterCacheKeyedOnConcreteProvider documents the identity assumption the
// cache relies on: after the first SetMeterProvider, otel's global returns the
// concrete provider that was set, so each swap is a distinct, comparable key.
func TestMeterCacheKeyedOnConcreteProvider(t *testing.T) {
	// Ensure the global has been set at least once before, so we're past
	// otel's one-shot delegation and GetMeterProvider returns concretes.
	newTestMeterReader(t)

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	require.Same(t, provider, otel.GetMeterProvider(),
		"GetMeterProvider must return the concrete provider so identity keying detects swaps")
}
