package service

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

func TestCompareMixed(t *testing.T) {
	got := []string{"10", "1x", "", "2"}
	slices.SortStableFunc(got, compare)
	if want := []string{"2", "10", "", "1x"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// serialStore is a store whose cells in dated columns hold Sheets serial numbers, which Read shows
// formatted as a US-locale spreadsheet would.
type serialStore struct {
	rows    [][]any
	serials map[int][]any // by column, from row 1
	written [][]any
}

func (f *serialStore) Read(context.Context, string, string) ([][]any, error) { return f.rows, nil }
func (f *serialStore) ReadSerial(_ context.Context, _, _ string, cols []int) ([][]any, error) {
	var out [][]any
	for _, c := range cols {
		out = append(out, f.serials[c])
	}
	return out, nil
}

func (f *serialStore) Append(_ context.Context, _, _ string, row []any) error {
	f.written = append(f.written, row)
	return nil
}
func (f *serialStore) SetCells(context.Context, string, string, int, map[int]any) error { return nil }
func (f *serialStore) DeleteRow(context.Context, string, string, int) error             { return nil }
func (f *serialStore) Tabs(context.Context, string) ([]string, error)                   { return nil, nil }

// Dates read as ISO 8601 whatever the spreadsheet's locale, filters compare by column type, and
// date values are written as store.Dates.
func TestDatesAndTypedFilters(t *testing.T) {
	cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
		"Tasks": {Ops: db.AllOps, Columns: map[string]db.Column{
			"due": {Type: "date"}, "at": {Type: "datetime"}, "points": {Type: "integer"}, "done": {Type: "boolean"},
		}},
	}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st := &serialStore{
		rows: [][]any{
			{"id", "due", "at", "points", "done"},
			{"a", "11/5/2026", "11/5/2026 9:30:00", 8.0, false},
			{"b", "soon", "", 5.0, "TRUE"},
		},
		serials: map[int][]any{1: {"due", 46331.0, "soon"}, 2: {"at", 46331.395833333336}},
	}
	svc := New(cfg, st, 0)
	ctx := context.Background()
	rows, err := svc.List(ctx, "shop", "Tasks", nil, "", 0, 0)
	if err != nil || len(rows) != 2 || rows[0]["due"] != "2026-11-05" || rows[0]["at"] != "2026-11-05T09:30:00Z" || rows[1]["due"] != "soon" {
		t.Fatalf("list: %v %v", rows, err)
	}
	for filter, want := range map[string][]string{
		"points=8.0":                       {"a"},
		"points[ne]=8":                     {"b"},
		"done=TRUE":                        {"b"},
		"done=false":                       {"a"},
		"due[gte]=2026-11-01":              {"a", "b"}, // "soon" sorts after ISO dates as a string
		"at[lt]=2026-11-05T12:00:00+02:00": {"a"},
		"points[contains]=8":               {"a"},
	} {
		k, v, _ := strings.Cut(filter, "=")
		got, err := svc.List(ctx, "shop", "Tasks", map[string]string{k: v}, "", 0, 0)
		var ids []string
		for _, r := range got {
			ids = append(ids, r["id"].(string))
		}
		if err != nil || !slices.Equal(ids, want) {
			t.Errorf("filter %s: %v %v, want %v", filter, ids, err, want)
		}
	}
	for filter, msg := range map[string]string{
		"points[gt]=lots": "points[gt] must be a number",
		"done=yes":        "done must be true or false",
		"due=11/5/2026":   "due must be a date like 2026-01-31",
	} {
		k, v, _ := strings.Cut(filter, "=")
		if _, err := svc.List(ctx, "shop", "Tasks", map[string]string{k: v}, "", 0, 0); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("filter %s: %v, want %q", filter, err, msg)
		}
	}

	row, err := svc.Create(ctx, "shop", "Tasks", map[string]any{"due": "2026-12-01", "at": "2026-12-01T10:00:00+01:00", "points": json.Number("2")})
	if err != nil || row["at"] != "2026-12-01T09:00:00Z" || row["due"] != "2026-12-01" {
		t.Fatalf("create: %v %v", row, err)
	}
	w := st.written[0]
	if d, ok := w[1].(store.Date); !ok || d.WithTime || d.Format(time.DateOnly) != "2026-12-01" {
		t.Errorf("due written as %#v", w[1])
	}
	if d, ok := w[2].(store.Date); !ok || !d.WithTime || !d.Equal(time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("at written as %#v", w[2])
	}
}

func TestFromSerial(t *testing.T) {
	for _, c := range []struct {
		days     float64
		withTime bool
		want     string
	}{
		{0, false, "1899-12-30"},
		{1, false, "1899-12-31"},
		{46331, false, "2026-11-05"},
		{46331.5, true, "2026-11-05T12:00:00Z"},
		{46331.999999999, true, "2026-11-06T00:00:00Z"},
	} {
		if got := fromSerial(c.days, c.withTime); got != c.want {
			t.Errorf("fromSerial(%v, %v) = %s, want %s", c.days, c.withTime, got, c.want)
		}
	}
}
