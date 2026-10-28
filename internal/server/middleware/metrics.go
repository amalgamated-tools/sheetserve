package middleware

import (
	"net/http"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// requestDurationBucketBoundaries are the semconv-recommended bucket
// boundaries (in seconds) for http.server.request.duration. The SDK's
// default boundaries are tuned for millisecond-scale measurements — without
// this override, everything under 5s (i.e. nearly every request this
// service handles) would collapse into the histogram's first bucket and the
// latency distribution would carry no information.
var requestDurationBucketBoundaries = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// httpMetricInstruments returns the request-duration histogram and
// active-requests counter for the current global MeterProvider. The handles
// are cached per provider (see meterCache), so the steady-state per-request
// cost is an atomic load plus a map read rather than the SDK's registry
// locks; when the provider is swapped (SetupMetrics at boot, or the tests'
// per-case providers) the cache rebuilds against it, so recordings always
// land on whichever provider is installed now.
func httpMetricInstruments() (metric.Float64Histogram, metric.Int64UpDownCounter) {
	cache := otel.ActiveMeterCache()

	duration, err := cache.Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of inbound HTTP requests, from when MetricsMiddleware invokes the rest of the chain until the handler returns."),
		metric.WithExplicitBucketBoundaries(requestDurationBucketBoundaries...),
	)
	if err != nil {
		otel.LogCreateErrorOnce("histogram", "http.server.request.duration", err)
	}

	active, err := cache.UpDownCounter(
		"http.server.active_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Number of HTTP requests currently being served."),
	)
	if err != nil {
		otel.LogCreateErrorOnce("counter", "http.server.active_requests", err)
	}

	return duration, active
}

// Metrics records standard HTTP server metrics for every request:
// an in-flight count (http.server.active_requests, an up-down counter) and a
// duration histogram (http.server.request.duration), both broken down by
// method, route, and (for the histogram) response status code. The route
// label comes from http.Request.Pattern via routeLabel, which net/http's
// ServeMux populates once it matches a registered pattern — so it's only
// available after next.ServeHTTP returns, and stays "unmatched" for
// requests no route claimed (e.g. 404s).
//
// It calls RecorderFor rather than always wrapping w itself, so
// when this runs right after TraceMiddleware (as it does in
// internal/server.Server.Run) both share the one recorder TraceMiddleware
// already created instead of nesting a second wrapper.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duration, active := httpMetricInstruments()

		ctx := r.Context()
		method := semconv.HTTPRequestMethodKey.String(r.Method)

		active.Add(ctx, 1, metric.WithAttributes(method))
		defer active.Add(ctx, -1, metric.WithAttributes(method))

		start := time.Now()
		rec := RecorderFor(w)

		// Deferred, not called plainly after next.ServeHTTP, so a panicking
		// handler still produces a data point: net/http's ServeMux sets
		// r.Pattern before invoking the handler, so the route label is
		// already correct by panic time, and this defer runs during the
		// panic's unwind (without recovering it) before net/http's own
		// top-level recovery further up the stack. The status code, though,
		// reflects whatever was written before the panic (defaulting to 200
		// if nothing was), not the 500 net/http ultimately sends the client.
		defer func() {
			duration.Record(
				ctx,
				time.Since(start).Seconds(),
				metric.WithAttributes(
					method,
					semconv.HTTPRoute(routeLabel(r)),
					semconv.HTTPResponseStatusCode(rec.StatusCode),
				),
			)
		}()

		next.ServeHTTP(rec, r)
	})
}
