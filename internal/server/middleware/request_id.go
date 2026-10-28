package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/amalgamated-tools/sheetserve/internal/otel"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type contextKey string

// RequestID is the HTTP header name used to propagate request identifiers
// between clients and the server. Incoming requests that already carry this
// header have their value preserved; otherwise a new UUID is generated. If
// UUID generation fails, the ID falls back to the literal string "none".
const RequestID = "X-Request-ID"

// ctxRequestIDKey is the unexported context key for the X-Request-ID value.
const ctxRequestIDKey contextKey = "go-http-RequestId"

// Every *Context log call automatically carries the request ID, so handlers
// never need to attach it manually.
func init() {
	otel.RegisterContextLogAttr(func(ctx context.Context) (slog.Attr, bool) {
		if id := GetRequestID(ctx); id != "" {
			return slog.String(otelkeys.RequestID, id), true
		}

		return slog.Attr{}, false
	})
}

// RequestIDHandler is a middleware that generates or extracts request IDs and adds them to the request context.
func RequestIDHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Health checks get no span (see isHealthCheck); this runs first in the
		// chain, so without the skip its span would be the root of a trace per
		// probe.
		if isHealthCheck(r) {
			next.ServeHTTP(w, r)

			return
		}

		newctx, span := otel.StartSpan(r.Context(), instrumentationName, otelkeys.ServerMiddlewareRequestIDHandler)
		defer span.End()

		id := r.Header.Get(RequestID)

		if len(id) == 0 {
			if newID, err := uuid.NewRandom(); err != nil {
				otel.RecordError(span, "failed to generate request ID", err)
				slog.ErrorContext(newctx, "Failed to generate request ID", slog.String(otelkeys.Error, err.Error()))
				id = "none"
			} else {
				id = newID.String()
			}
		}

		span.SetAttributes(attribute.String(otelkeys.RequestID, id))
		r = r.WithContext(WithRequestID(newctx, id))

		// ensure the request ID is also available to clients in the response headers
		w.Header().Set(RequestID, id)
		next.ServeHTTP(w, r)
		span.SetStatus(codes.Ok, "")
	}

	return http.HandlerFunc(fn)
}

// GetRequestID returns the request ID if one is present.
func GetRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if reqID, ok := ctx.Value(ctxRequestIDKey).(string); ok {
		return reqID
	}

	return ""
}

// WithRequestID creates a new context based on the supplied parent, with the RequestId
// set to the specified value. If a RequestID already exists, it will be overwritten.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestIDKey, id)
}
