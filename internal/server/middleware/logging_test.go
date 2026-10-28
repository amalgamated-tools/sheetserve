package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
)

func TestLoggingMiddleware(t *testing.T) {
	t.Parallel()
	t.Run("passes request through and preserves status", func(t *testing.T) {
		t.Parallel()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("short and stout"))
		})

		r := httptest.NewRequest(http.MethodGet, "/brew?flavor=earl-grey", nil)
		w := httptest.NewRecorder()
		LoggingMiddleware(next).ServeHTTP(w, r)

		require.Equal(t, http.StatusTeapot, w.Code)
		require.Equal(t, "short and stout", w.Body.String())
	})

	t.Run("defaults to 200 when handler never writes header", func(t *testing.T) {
		t.Parallel()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
		})

		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		LoggingMiddleware(next).ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code)
	})
}

// Each request logs one line with its path but not its query string; a 5xx logs at error.
func TestLoggingMiddlewareLogsRequest(t *testing.T) {
	logs := oteltest.CaptureLogs(t)
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		r := httptest.NewRequest(http.MethodPost, "/v1/shop/Orders?email=ann@example.com", nil)
		LoggingMiddleware(next).ServeHTTP(httptest.NewRecorder(), r)
	}

	recs := logs("http request")
	require.Len(t, recs, 2)
	require.Equal(t, "INFO", recs[0]["level"])
	require.Equal(t, "ERROR", recs[1]["level"])
	for i, status := range []float64{200, 502} {
		require.Equal(t, "POST", recs[i]["method"])
		require.Equal(t, "/v1/shop/Orders", recs[i]["path"])
		require.Equal(t, status, recs[i]["status_code"])
		require.Contains(t, recs[i], "duration_ms")
	}
}

func TestGetRequestID_MissingValues(t *testing.T) {
	t.Parallel()
	require.Empty(t, GetRequestID(t.Context()))
}
