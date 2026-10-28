package middleware

import (
	"net/http"

	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// LoggingMiddleware returns an HTTP middleware that emits a structured debug
// log entry at the start of each request (method, URL, remote address, user
// agent, request ID, and user ID) and a second entry upon completion
// (method, URL, status code, elapsed duration, request ID, and user ID).
func LoggingMiddleware(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Before StartSpan: Trace skips health checks, so a span started here
		// would have no parent and export as its own root trace.
		if isHealthCheck(r) {
			next.ServeHTTP(w, r)

			return
		}

		newctx, span := otel.StartSpan(r.Context(), instrumentationName, otelkeys.ServerMiddlewareLoggingMiddleware)
		defer span.End()
		defer otel.ObserveDuration(newctx, otelkeys.ServerMiddlewareLoggingMiddlewareDuration)()

		requestURL := r.URL.String()

		attrs := []attribute.KeyValue{
			attribute.String(otelkeys.Method, r.Method),
			attribute.String(otelkeys.URL, requestURL),
			attribute.String(otelkeys.RemoteAddr, r.RemoteAddr),
			attribute.String(otelkeys.UserAgent, r.UserAgent()),
			attribute.String(otelkeys.RequestID, GetRequestID(newctx)),
		}
		span.SetAttributes(attrs...)

		rec := RecorderFor(w)
		next.ServeHTTP(rec, r)
		span.SetAttributes(attribute.Int(otelkeys.StatusCode, rec.StatusCode))
		if rec.StatusCode >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rec.StatusCode))
		} else {
			span.SetStatus(codes.Ok, "")
			otel.Incr(
				newctx,
				otelkeys.ServerMiddlewareLoggingMiddlewareSuccess,
			)
		}
	}

	return http.HandlerFunc(fn)
}
