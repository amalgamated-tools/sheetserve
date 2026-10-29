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

// Every string goes in as text, so nothing a client sends becomes a formula, number or date.
// Only a Date becomes a date cell.
func TestUserEntered(t *testing.T) {
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ in, want any }{
		{"=IMPORTXML(A1)", "'=IMPORTXML(A1)"},
		{"2026-11-05", "'2026-11-05"},
		{"007", "'007"},
		{"'quoted", "''quoted"},
		{"", ""},
		{3.5, 3.5},
		{true, true},
		{nil, nil},
		{Date{Time: day}, "2026-11-05"},
		{Date{Time: time.Date(2026, 11, 5, 9, 30, 0, 0, time.FixedZone("", 2*60*60)), WithTime: true}, "2026-11-05 07:30:00"},
	} {
		if got := userEntered(c.in); got != c.want {
			t.Errorf("userEntered(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
