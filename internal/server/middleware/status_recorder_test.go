package middleware

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatusRecorder_Write(t *testing.T) {
	rec := &StatusRecorder{ResponseWriter: httptest.NewRecorder()}

	n, err := rec.Write([]byte("body"))
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, http.StatusOK, rec.StatusCode, "Write before WriteHeader should default the status to 200")
}

func TestStatusRecorder_WriteHeader(t *testing.T) {
	under := httptest.NewRecorder()
	rec := &StatusRecorder{ResponseWriter: under}

	rec.WriteHeader(http.StatusNotFound)
	require.Equal(t, http.StatusNotFound, rec.StatusCode)
	require.Equal(t, http.StatusNotFound, under.Code)
}

func TestStatusRecorder_Flush(t *testing.T) {
	t.Run("delegates to a flushing writer", func(t *testing.T) {
		under := httptest.NewRecorder()
		rec := &StatusRecorder{ResponseWriter: under}

		rec.Flush()
		require.True(t, under.Flushed)
	})

	t.Run("is a no-op for non-flushing writers", func(t *testing.T) {
		rec := &StatusRecorder{ResponseWriter: plainResponseWriter{}}
		rec.Flush() // must not panic
	})
}

func TestStatusRecorder_Hijack(t *testing.T) {
	t.Run("returns ErrNotSupported for non-hijackable writers", func(t *testing.T) {
		rec := &StatusRecorder{ResponseWriter: httptest.NewRecorder()}

		conn, rw, err := rec.Hijack()
		require.ErrorIs(t, err, http.ErrNotSupported)
		require.Nil(t, conn)
		require.Nil(t, rw)
	})

	t.Run("delegates to a hijackable writer", func(t *testing.T) {
		under := &hijackableResponseWriter{ResponseWriter: httptest.NewRecorder()}
		rec := &StatusRecorder{ResponseWriter: under}

		_, _, err := rec.Hijack()
		require.NoError(t, err)
		require.True(t, under.hijacked)
	})
}

func TestStatusRecorder_Push(t *testing.T) {
	t.Run("returns ErrNotSupported for non-pushing writers", func(t *testing.T) {
		rec := &StatusRecorder{ResponseWriter: httptest.NewRecorder()}

		err := rec.Push("/asset.css", nil)
		require.ErrorIs(t, err, http.ErrNotSupported)
	})

	t.Run("delegates to a pushing writer", func(t *testing.T) {
		under := &pushableResponseWriter{ResponseWriter: httptest.NewRecorder()}
		rec := &StatusRecorder{ResponseWriter: under}

		require.NoError(t, rec.Push("/asset.css", nil))
		require.Equal(t, "/asset.css", under.pushedTarget)
	})
}

func TestRecorderFor(t *testing.T) {
	t.Run("wraps a plain ResponseWriter", func(t *testing.T) {
		under := httptest.NewRecorder()
		rec := RecorderFor(under)
		require.NotNil(t, rec)
		require.Equal(t, http.StatusOK, rec.StatusCode)
	})

	t.Run("reuses an existing StatusRecorder instead of wrapping again", func(t *testing.T) {
		under := httptest.NewRecorder()
		original := NewStatusRecorder(under)

		reused := RecorderFor(original)
		require.Same(t, original, reused, "RecorderFor should return the exact same recorder, not a new wrapper")

		// A status set through the first-created recorder must be visible
		// through the "reused" reference too, since they're the same object.
		original.WriteHeader(http.StatusTeapot)
		require.Equal(t, http.StatusTeapot, reused.StatusCode)
	})
}

// plainResponseWriter implements only http.ResponseWriter, with none of the
// optional interfaces.
type plainResponseWriter struct{}

func (plainResponseWriter) Header() http.Header         { return http.Header{} }
func (plainResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (plainResponseWriter) WriteHeader(int)             {}

type hijackableResponseWriter struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

type pushableResponseWriter struct {
	http.ResponseWriter
	pushedTarget string
}

func (p *pushableResponseWriter) Push(target string, opts *http.PushOptions) error {
	p.pushedTarget = target
	return nil
}
