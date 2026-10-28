package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestGetRequestID_MissingValues(t *testing.T) {
	t.Parallel()
	require.Empty(t, GetRequestID(t.Context()))
}
