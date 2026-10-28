package middleware

import (
	"net/http"
	"strings"

	otelpkg "github.com/amalgamated-tools/sheetserve/internal/otel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Trace is a middleware that adds tracing to HTTP requests. Each
// request gets its own server span, tagged with HTTP semantic-convention
// attributes (method, path, user agent) up front and the route/status code
// (and an error status for 5xxs) once the handler chain returns.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health checks are high-frequency and low-value as traces, so skip
		// span creation entirely for them. RequestIDHandler and
		// LoggingMiddleware skip before their StartSpan too; otherwise a span
		// from either would become a root trace per health check.
		if isHealthCheck(r) {
			next.ServeHTTP(w, r)

			return
		}

		// Extract any inbound W3C trace context so a caller-initiated trace
		// continues through this request rather than starting a new root.
		ctx := otel.GetTextMapPropagator().Extract(
			r.Context(),
			propagation.HeaderCarrier(r.Header),
		)

		// Each HTTP request gets its own span.
		spanName := r.Method
		if r.URL != nil {
			spanName += " " + r.URL.Path
		}

		ctx, span := otelpkg.StartSpan(
			ctx,
			instrumentationName,
			spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.URLPath(r.URL.Path),
				semconv.UserAgentOriginal(r.UserAgent()),
			),
		)
		defer span.End()

		// req is the exact *http.Request passed downstream; net/http's
		// ServeMux mutates its Pattern field in place once it matches a
		// route, so reading req.Pattern (via routeLabel) only after
		// next.ServeHTTP returns (and only off this same pointer, not the
		// original r) picks that up. TraceMiddleware runs first in the
		// chain (internal/server.Server.Run), so RecorderFor
		// creates the one recorder here; MetricsMiddleware immediately
		// downstream reuses this same instance via its own RecorderFor
		// call instead of wrapping again.
		req := r.WithContext(ctx)
		rec := RecorderFor(w)

		// Deferred rather than called plainly after next.ServeHTTP, so a
		// panicking handler still gets its route/status attributes set (this
		// runs during the panic's unwind, before net/http's own top-level
		// recovery further up the stack, without recovering the panic
		// itself). The status code, though, reflects whatever was written
		// before the panic (defaulting to 200 if nothing was), not the 500
		// net/http ultimately sends the client.
		defer func() {
			span.SetAttributes(
				semconv.HTTPRoute(routeLabel(req)),
				semconv.HTTPResponseStatusCode(rec.StatusCode),
			)
			if rec.StatusCode >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.StatusCode))
			}
		}()

		next.ServeHTTP(rec, req)
	})
}

// routeLabel returns r.Pattern, the pattern net/http's ServeMux matched
// against this request, or "unmatched" if no registered route claimed it
// (e.g. a 404). r.Pattern is only populated once ServeMux has run, so this
// is only meaningful after the request has been passed down the handler
// chain — shared by TraceMiddleware and MetricsMiddleware so the fallback
// label can't drift between the two.
func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched"
	}

	return r.Pattern
}

// isHealthCheck reports whether r is a /healthz probe. Every middleware
// that starts a span checks this before StartSpan, so the skip holds
// across the whole chain in internal/server.Server.Run rather than
// drifting between copies of the path check.
func isHealthCheck(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/healthz")
}
