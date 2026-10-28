// Package middleware holds small http.ResponseWriter helpers shared by more
// than one middleware package, so they have one implementation instead of
// drifting copies.
package middleware

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
)

// StatusRecorder wraps http.ResponseWriter to capture the status code a
// handler writes, for logging, tracing, or metrics middleware that needs it
// after the handler returns.
type StatusRecorder struct {
	http.ResponseWriter

	StatusCode int
}

// NewStatusRecorder returns a StatusRecorder defaulting to 200 OK, matching
// net/http's implicit behavior when a handler writes a body without ever
// calling WriteHeader.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, StatusCode: http.StatusOK}
}

// RecorderFor returns w as a *StatusRecorder without adding another wrapper
// layer if w already is one (typically because an earlier middleware in the
// same chain already wrapped it) — otherwise it wraps w fresh. Middleware
// that sits back-to-back in a chain (e.g. tracing then metrics) should call
// this instead of NewStatusRecorder so only one recorder, not one per
// middleware, wraps a given request.
func RecorderFor(w http.ResponseWriter) *StatusRecorder {
	if rec, ok := w.(*StatusRecorder); ok {
		return rec
	}

	return NewStatusRecorder(w)
}

// WriteHeader captures the status code before delegating to the underlying
// ResponseWriter.
func (r *StatusRecorder) WriteHeader(code int) {
	r.StatusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// Write defaults the captured status code to 200 OK when WriteHeader was not
// called before the first write, then delegates to the underlying ResponseWriter.
func (r *StatusRecorder) Write(b []byte) (int, error) {
	if r.StatusCode == 0 {
		r.StatusCode = http.StatusOK
	}

	i, err := r.ResponseWriter.Write(b)
	if err != nil {
		err = fmt.Errorf("failed to write response: %w", err)
	}
	return i, err
}

// Flush delegates to the underlying ResponseWriter if it implements http.Flusher.
func (r *StatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying ResponseWriter if it implements http.Hijacker.
func (r *StatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}

	c, rw, err := h.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to hijack connection: %w", err)
	}
	return c, rw, nil
}

// Push delegates to the underlying ResponseWriter if it implements http.Pusher.
func (r *StatusRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}

	err := p.Push(target, opts)
	if err != nil {
		return fmt.Errorf("failed to push: %w", err)
	}
	return nil
}
