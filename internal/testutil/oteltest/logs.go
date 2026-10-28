package oteltest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// LogRecord is one captured log line, decoded from JSON: "msg", "level" and
// each attribute by key.
type LogRecord map[string]any

// CaptureLogs points slog's default logger at an in-memory JSON handler at
// debug level until the test ends. It changes global state, so a test that
// calls it must not run in parallel. The returned func gives the records
// logged so far with the given message.
func CaptureLogs(t *testing.T) func(msg string) []LogRecord {
	t.Helper()

	var mu sync.Mutex
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return func(msg string) []LogRecord {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		var out []LogRecord
		for line := range strings.Lines(buf.String()) {
			var rec LogRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			if rec["msg"] == msg {
				out = append(out, rec)
			}
		}
		return out
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
