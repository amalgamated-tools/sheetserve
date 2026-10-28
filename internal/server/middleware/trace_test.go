//nolint:paralleltest // these tests install global process state
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	otelpkg "github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/justinas/alice"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// These tests install global process state (tracer/meter providers), so they
// run without t.Parallel.

func TestTraceExtractsInboundContext(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	shutdown, err := otelpkg.SetupTracing(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shutdown(context.Background())) })

	// Start a caller-side span and inject its context into request headers,
	// mimicking an upstream that already has a trace in flight.
	_, parent := otelpkg.StartSpan(context.Background(), instrumentationName, "parent")
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	otel.GetTextMapPropagator().Inject(
		trace.ContextWithSpan(context.Background(), parent),
		propagation.HeaderCarrier(req.Header),
	)
	parent.End()

	// Drive an actual request through Trace and assert the handler's span
	// descends from the injected trace — i.e. the Extract wiring works.
	var childTraceID trace.TraceID
	handler := Trace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		childTraceID = trace.SpanContextFromContext(r.Context()).TraceID()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.Equal(
		t,
		parent.SpanContext().TraceID(),
		childTraceID,
		"middleware should continue the inbound trace",
	)
}

func TestTraceRecordsRouteAndStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	// Trace must wrap the mux (not the other way around) so that r.Pattern
	// is populated by the time it reads it back.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /brew", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := Trace(mux)

	req := httptest.NewRequest(http.MethodGet, "/brew", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attribute.NewSet(spans[0].Attributes...)

	route, ok := attrs.Value(semconv.HTTPRouteKey)
	require.True(t, ok, "expected an http.route attribute")
	require.Equal(t, "GET /brew", route.AsString())

	status, ok := attrs.Value(semconv.HTTPResponseStatusCodeKey)
	require.True(t, ok, "expected an http.response.status_code attribute")
	require.EqualValues(t, http.StatusTeapot, status.AsInt64())
}

// TestHealthzProducesNoSpansAcrossChain mirrors the chain in
// internal/server.Server.Run. Wrapping a bare handler in Trace alone cannot
// catch the spans RequestIDHandler and LoggingMiddleware start themselves;
// a /healthz probe through the real chain must export nothing.
func TestHealthzProducesNoSpansAcrossChain(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	router := chi.NewRouter()
	router.Use(chimiddleware.Heartbeat("/healthz"))
	router.Get("/brew", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	chain := alice.New(
		RequestIDHandler,
		Trace,
		Metrics,
		chimiddleware.Recoverer,
		chimiddleware.URLFormat,
		LoggingMiddleware,
	).Then(router)

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code, "health check should still be served")
	require.Empty(t, exporter.GetSpans(), "a health check must not export a span from any middleware")

	// The same chain traces an ordinary request, so the assertion above is
	// not vacuous.
	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/brew", nil))
	require.NotEmpty(t, exporter.GetSpans())
}

func TestTraceUnmatchedRouteFallsBackToUnmatched(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	handler := Trace(http.NewServeMux()) // no routes registered

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attribute.NewSet(spans[0].Attributes...)

	route, ok := attrs.Value(semconv.HTTPRouteKey)
	require.True(t, ok)
	require.Equal(t, "unmatched", route.AsString())
}

func TestTraceMarksServerErrorSpanStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	handler := Trace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status.Code, "5xx responses should mark the span as an error")

	// A successful request must NOT be marked as an error.
	exporter.Reset()
	okHandler := Trace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	okHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))

	spans = exporter.GetSpans()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Unset, spans[0].Status.Code)
}

func TestTraceRecordsRouteEvenWhenHandlerPanics(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /brew", func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	handler := Trace(mux)

	req := httptest.NewRequest(http.MethodGet, "/brew", nil)
	require.PanicsWithValue(t, "boom", func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}, "the middleware must not swallow the panic")

	// The span is still closed (via defer span.End()) and still carries the
	// route attribute (net/http's ServeMux sets r.Pattern before invoking
	// the handler, so it survives the panic) even though the handler never
	// returned normally.
	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attribute.NewSet(spans[0].Attributes...)

	route, ok := attrs.Value(semconv.HTTPRouteKey)
	require.True(t, ok)
	require.Equal(t, "GET /brew", route.AsString())
}

// TestTraceAndMetricsShareOneStatusRecorder is a regression test for the
// chain wiring in internal/server.Server.Run: Trace runs immediately before
// Metrics, and both must resolve to the exact same *StatusRecorder (via
// RecorderFor) rather than each wrapping the ResponseWriter again.
func TestTraceAndMetricsShareOneStatusRecorder(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	shutdownTracing, err := otelpkg.SetupTracing(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shutdownTracing(context.Background())) })
	reader := oteltest.InstallMeterReader(t)

	var sawWriter http.ResponseWriter
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sawWriter = w
		w.WriteHeader(http.StatusTeapot)
	})

	handler := Trace(Metrics(inner))
	req := httptest.NewRequest(http.MethodGet, "/brew", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rec, ok := sawWriter.(*StatusRecorder)
	require.True(t, ok, "the innermost handler should see the shared *StatusRecorder")
	require.Equal(t, http.StatusTeapot, rec.StatusCode)
	_, nested := rec.ResponseWriter.(*StatusRecorder)
	require.False(t, nested, "Trace and Metrics should not create nested status recorders")

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	m := oteltest.FindMetric(t, data, "http.server.request.duration")
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 1)
	status, ok := hist.DataPoints[0].Attributes.Value(semconv.HTTPResponseStatusCodeKey)
	require.True(t, ok, "expected an http.response.status_code attribute")
	require.EqualValues(t, http.StatusTeapot, status.AsInt64())
}
