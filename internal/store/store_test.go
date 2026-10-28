package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
)

func TestColName(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		if got := colName(i); got != want {
			t.Errorf("colName(%d) = %s, want %s", i, got, want)
		}
	}
}

// A Google call logs its op, duration, ids and any error at debug.
func TestLogCall(t *testing.T) {
	logs := oteltest.CaptureLogs(t)
	var ok, failed error = nil, errors.New("quota exceeded")
	logCall(context.Background(), "read", "SS", "Orders", time.Now(), &ok)
	logCall(context.Background(), "create", "", "", time.Now(), &failed)

	recs := logs("google sheets call")
	if len(recs) != 2 {
		t.Fatalf("records: %v", recs)
	}
	if r := recs[0]; r["level"] != "DEBUG" || r["op"] != "read" || r["spreadsheet_id"] != "SS" || r["tab"] != "Orders" || r["duration_ms"] == nil || r["error"] != nil {
		t.Errorf("read record: %v", r)
	}
	if r := recs[1]; r["op"] != "create" || r["error"] != "quota exceeded" || r["spreadsheet_id"] != nil || r["tab"] != nil {
		t.Errorf("create record: %v", r)
	}
}
