package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// LoggingMiddleware returns an HTTP middleware that records each request on a
// span and logs one "http request" entry when it completes: method, path,
// status code and duration, plus the request and trace IDs every *Context log
// carries. A 5xx response logs at error, anything else at info. The query
// string is left out because API filters can hold personal data.
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

		start := time.Now()
		rec := RecorderFor(w)
		next.ServeHTTP(rec, r)
		span.SetAttributes(attribute.Int(otelkeys.StatusCode, rec.StatusCode))

		level := slog.LevelInfo
		if rec.StatusCode >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		slog.LogAttrs(newctx, level, "http request",
			slog.String(otelkeys.Method, r.Method),
			slog.String(otelkeys.Path, r.URL.Path),
			slog.Int(otelkeys.StatusCode, rec.StatusCode),
			slog.Int64(otelkeys.DurationMs, time.Since(start).Milliseconds()),
		)

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
