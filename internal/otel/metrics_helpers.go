package otel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// createErrorOnce dedups instrument-creation error logs, keyed by
// "<kind> <name>" so a counter and histogram sharing a name don't collide.
var createErrorOnce sync.Map

// LogCreateErrorOnce logs a failed instrument creation exactly once per
// kind/name pair; kind is the instrument noun ("counter", "histogram").
func LogCreateErrorOnce(kind, name string, err error) {
	v, _ := createErrorOnce.LoadOrStore(kind+" "+name, &sync.Once{})
	vsync, ok := v.(*sync.Once)
	if !ok {
		panic("createErrorOnce contains non-*sync.Once value")
	}
	vsync.Do(func() {
		slog.Error("failed to create instrument",
			slog.String(otelkeys.Kind, kind),
			slog.String(otelkeys.Name, name),
			slog.Any(otelkeys.Error, err))
	})
}

// RegisterInt64Gauge registers an asynchronous Int64 gauge whose data points
// are produced by observe on every metric collection. Use it for levels — a
// current count, or a config value reflected as telemetry — where the
// monotonic / running-total semantics of Add would be wrong: a gauge reports
// the current state each collection, so it neither accumulates across process
// restarts nor decays to zero on a rate() graph.
//
// The callback is retained by the MeterProvider installed when this is called,
// for the process lifetime, so register once after telemetry setup. Observable
// instruments are registered rather than resolved per recording, so unlike the
// synchronous helpers this does not go through meterCache.
func RegisterInt64Gauge(name string, observe func(context.Context, metric.Int64Observer) error) {
	_, err := otel.GetMeterProvider().Meter(instrumentationName).Int64ObservableGauge(
		name, metric.WithInt64Callback(observe),
	)
	if err != nil {
		LogCreateErrorOnce("gauge", name, err)
	}
}

// Incr increments the named Int64 counter by 1, creating the meter/counter
// as needed from the current global MeterProvider.
func Incr(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	Add(ctx, name, 1, attrs...)
}

// Add increments the named Int64 counter by delta, reusing the cached
// counter handle for the current global MeterProvider (see meterCache).
func Add(ctx context.Context, name string, delta int64, attrs ...attribute.KeyValue) {
	counter, err := ActiveMeterCache().counter(name)
	if err != nil {
		LogCreateErrorOnce("counter", name, err)

		return
	}
	if len(attrs) == 0 {
		counter.Add(ctx, delta)

		return
	}
	counter.Add(ctx, delta, metric.WithAttributes(attrs...))
}

// ObserveDuration returns a function suitable for defer that records elapsed
// duration, in seconds, to a Float64 histogram with the given metric name.
//
// Usage:
//
//	defer otel.ObserveDuration(ctx, "my.function.duration")()
func ObserveDuration(ctx context.Context, name string, attrs ...attribute.KeyValue) func() {
	start := time.Now()

	return func() {
		// Snapshot the elapsed time before touching the meter, so the cost
		// of resolving the histogram handle never leaks into the value we
		// record as the observed duration.
		value := time.Since(start).Seconds()
		hist, err := ActiveMeterCache().Histogram(name, metric.WithUnit("s"))
		if err != nil {
			LogCreateErrorOnce("histogram", name, err)

			return
		}
		if len(attrs) == 0 {
			hist.Record(ctx, value)

			return
		}
		hist.Record(ctx, value, metric.WithAttributes(attrs...))
	}
}
