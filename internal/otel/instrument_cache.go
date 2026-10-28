package otel

import (
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MeterCache caches OpenTelemetry metric instruments to avoid redundant creation.
type MeterCache struct {
	provider   metric.MeterProvider
	counters   sync.Map // name -> metric.Int64Counter
	upDowns    sync.Map // name -> metric.Int64UpDownCounter
	histograms sync.Map // name -> metric.Float64Histogram
}

var currentMeterCache atomic.Pointer[MeterCache]

// ActiveMeterCache returns the currently active MeterCache, creating a new one if necessary.
func ActiveMeterCache() *MeterCache {
	mp := otel.GetMeterProvider()
	if c := currentMeterCache.Load(); c != nil && c.provider == mp {
		return c
	}
	c := &MeterCache{provider: mp}
	currentMeterCache.Store(c)

	return c
}

func cachedInstrument[T any](m *sync.Map, name string, create func() (T, error)) (T, error) {
	if v, ok := m.Load(name); ok {
		vt, ok := v.(T)
		if ok {
			return vt, nil
		}
	}
	inst, err := create()
	if err != nil {
		return inst, err
	}
	actual, _ := m.LoadOrStore(name, inst)
	if actual == nil {
		return inst, nil
	}
	actualT, ok := actual.(T)
	if !ok {
		panic("cached instrument has unexpected type")
	}

	return actualT, nil
}

// UpDownCounter retrieves or creates a cached Int64UpDownCounter instrument.
func (c *MeterCache) UpDownCounter(name string, opts ...metric.Int64UpDownCounterOption) (metric.Int64UpDownCounter, error) {
	return cachedInstrument(&c.upDowns, name, func() (metric.Int64UpDownCounter, error) {
		return c.provider.Meter(instrumentationName).Int64UpDownCounter(name, opts...)
	})
}

// Histogram retrieves or creates a cached Float64Histogram instrument.
func (c *MeterCache) Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return cachedInstrument(&c.histograms, name, func() (metric.Float64Histogram, error) {
		return c.provider.Meter(instrumentationName).Float64Histogram(name, opts...)
	})
}

func (c *MeterCache) counter(name string) (metric.Int64Counter, error) {
	return cachedInstrument(&c.counters, name, func() (metric.Int64Counter, error) {
		return c.provider.Meter(instrumentationName).Int64Counter(name)
	})
}
