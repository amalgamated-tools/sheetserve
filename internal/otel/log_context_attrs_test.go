package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
)

// A context inside a span gives log records its trace ID; one outside a span gives none.
func TestTraceIDLogAttr(t *testing.T) {
	t.Parallel()
	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	inSpan := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
	}))

	traceIDs := func(ctx context.Context) []string {
		var out []string
		for _, fn := range *contextLogAttrFuncs.Load() {
			if attr, ok := fn(ctx); ok && attr.Key == otelkeys.TraceID {
				out = append(out, attr.Value.String())
			}
		}
		return out
	}
	require.Equal(t, []string{traceID.String()}, traceIDs(inSpan))
	require.Empty(t, traceIDs(context.Background()))
}
