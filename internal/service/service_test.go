package service

import (
	"context"
	"encoding/json"
	"reflect"
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
	reads   int // Read and ReadSerial calls
}

func (f *serialStore) Read(context.Context, string, string) ([][]any, error) {
	f.reads++
	return f.rows, nil
}

func (f *serialStore) ReadSerial(_ context.Context, _, _ string, cols []int) ([][]any, error) {
	f.reads++
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
	rows, _, err := svc.List(ctx, "shop", "Tasks", nil, "", 0, 0)
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
		got, _, err := svc.List(ctx, "shop", "Tasks", map[string]string{k: v}, "", 0, 0)
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
		if _, _, err := svc.List(ctx, "shop", "Tasks", map[string]string{k: v}, "", 0, 0); err == nil || !strings.Contains(err.Error(), msg) {
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

// A create fills columns it leaves out from their defaults, so a required column with a default
// can be left out. Enum filters must name one of the values.
func TestDefaultsOnCreate(t *testing.T) {
	cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
		"Orders": {Ops: db.AllOps, Hidden: []string{"source"}, Columns: map[string]db.Column{
			"status": {Type: "enum", Options: []string{"new", "paid"}, Required: true, Default: "new"},
			"qty":    {Type: "integer", Default: "1"},
			"source": {Default: "api"}, // hidden, but still filled in
		}},
	}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st := &serialStore{rows: [][]any{{"id", "status", "qty", "source"}}}
	svc := New(cfg, st, 0)
	ctx := context.Background()
	row, err := svc.Create(ctx, "shop", "Orders", map[string]any{})
	if err != nil || row["status"] != "new" || row["qty"] != json.Number("1") || row["source"] != nil {
		t.Fatalf("create with defaults: %v %v", row, err)
	}
	if w := st.written[0]; w[1] != "new" || w[2] != json.Number("1") || w[3] != "api" {
		t.Errorf("written: %v", w)
	}
	if row, err := svc.Create(ctx, "shop", "Orders", map[string]any{"status": "paid", "qty": nil}); err != nil || row["status"] != "paid" || row["qty"] != nil {
		t.Errorf("create overriding defaults: %v %v", row, err)
	}
	if _, err := svc.Create(ctx, "shop", "Orders", map[string]any{"status": "lost"}); err == nil || !strings.Contains(err.Error(), "status must be one of new, paid") {
		t.Errorf("bad enum: %v", err)
	}
	if _, _, err := svc.List(ctx, "shop", "Orders", map[string]string{"status": "lost"}, "", 0, 0); err == nil || !strings.Contains(err.Error(), "status must be one of new, paid") {
		t.Errorf("bad enum filter: %v", err)
	}
	if _, _, err := svc.List(ctx, "shop", "Orders", map[string]string{"status[contains]": "pa"}, "", 0, 0); err != nil {
		t.Errorf("enum contains: %v", err)
	}
}

// movingStore is a sheet where someone inserts a row above row 2 each time a cell is read, up to
// moves times, as a person editing by hand while the API writes might.
type movingStore struct {
	rows  [][]any
	moves int
	reads int
}

func (m *movingStore) Read(context.Context, string, string) ([][]any, error) {
	out := make([][]any, len(m.rows))
	for i, r := range m.rows {
		out[i] = slices.Clone(r)
	}
	return out, nil
}

func (m *movingStore) ReadCell(_ context.Context, _, _ string, row, col int) (any, error) {
	m.reads++
	if m.moves > 0 {
		m.moves--
		m.rows = slices.Insert(m.rows, 1, []any{"typed-by-hand", "x"})
	}
	if row-1 < len(m.rows) && col < len(m.rows[row-1]) {
		return m.rows[row-1][col], nil
	}
	return nil, nil
}

func (m *movingStore) SetCells(_ context.Context, _, _ string, row int, cells map[int]any) error {
	for c, v := range cells {
		m.rows[row-1][c] = v
	}
	return nil
}

func (m *movingStore) DeleteRow(_ context.Context, _, _ string, row int) error {
	m.rows = slices.Delete(m.rows, row-1, row)
	return nil
}
func (m *movingStore) Append(context.Context, string, string, []any) error { return nil }
func (m *movingStore) Tabs(context.Context, string) ([]string, error)      { return nil, nil }

// An update or delete reads its row's key back just before writing, and finds the row again when
// rows moved, so it never writes to the wrong one.
func TestLocateRowThatMoved(t *testing.T) {
	cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{"Orders": {Ops: db.AllOps}}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newStore := func(moves int) *movingStore {
		return &movingStore{moves: moves, rows: [][]any{{"id", "item"}, {"a", "tea"}, {"b", "mug"}}}
	}

	st := newStore(1)
	if _, err := New(cfg, st, 0).Update(ctx, "shop", "Orders", "b", map[string]any{"item": "cup"}); err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{"id", "item"}, {"typed-by-hand", "x"}, {"a", "tea"}, {"b", "cup"}}; !reflect.DeepEqual(st.rows, want) || st.reads != 2 {
		t.Errorf("update after a move: %v, %d reads", st.rows, st.reads)
	}

	st = newStore(2)
	if err := New(cfg, st, 0).Delete(ctx, "shop", "Orders", "a"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 4 || slices.ContainsFunc(st.rows, func(r []any) bool { return r[0] == "a" }) || st.rows[3][0] != "b" {
		t.Errorf("delete after two moves: %v", st.rows)
	}

	st = newStore(locateTries)
	if _, err := New(cfg, st, 0).Update(ctx, "shop", "Orders", "b", map[string]any{"item": "cup"}); err == nil || !strings.Contains(err.Error(), "kept moving") {
		t.Errorf("rows that keep moving: %v", err)
	}
	if slices.ContainsFunc(st.rows, func(r []any) bool { return r[1] == "cup" }) {
		t.Errorf("wrote while rows kept moving: %v", st.rows)
	}

	// A bad update is refused before any cell is read.
	st = newStore(0)
	if _, err := New(cfg, st, 0).Update(ctx, "shop", "Orders", "b", map[string]any{"nope": 1}); err == nil || st.reads != 0 {
		t.Errorf("bad field: %v, %d reads", err, st.reads)
	}
}

// Users who added the same spreadsheet, under different names and settings, share its cached tabs:
// each applies its own settings to the shared values without changing them, and a write by one
// drops the tab for both. A user reads the shared tab only while their own last read from Google is
// within the TTL, however recently someone else refreshed it.
func TestSharedCache(t *testing.T) {
	tasks := func(name string, rule db.TabRule) *db.Settings {
		cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{name: {ID: "SS", Tabs: map[string]db.TabRule{"Tasks": rule}}}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	st := &serialStore{
		rows:    [][]any{{"id", "due", "note"}, {"a", "11/5/2026", "hi"}},
		serials: map[int][]any{1: {"due", 46331.0}},
	}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cache := NewCache(time.Minute)
	cache.now = func() time.Time { return clock }
	alice := New(tasks("work", db.TabRule{Ops: db.AllOps, Columns: map[string]db.Column{"due": {Type: "date"}}}), st, 0)
	bob := New(tasks("team", db.TabRule{Ops: db.AllOps, Hidden: []string{"note"}}), st, 0)
	alice.UseCache(cache, 1)
	bob.UseCache(cache, 2)
	ctx := context.Background()
	list := func(svc *Service, sheet string) []map[string]any {
		t.Helper()
		rows, _, err := svc.List(ctx, sheet, "Tasks", nil, "", 0, 0)
		if err != nil || len(rows) == 0 {
			t.Fatalf("list %s: %v %v", sheet, rows, err)
		}
		return rows
	}

	// Bob reads the tab first, untyped, then Alice, who needs its dates: the cached values have no
	// serials, so she reads both again together.
	if b := list(bob, "team"); b[0]["due"] != "11/5/2026" || b[0]["note"] != nil || st.reads != 1 {
		t.Fatalf("bob: %v, %d reads", b, st.reads)
	}
	if a := list(alice, "work"); a[0]["due"] != "2026-11-05" || a[0]["note"] != "hi" || st.reads != 3 {
		t.Fatalf("alice: %v, %d reads, want 3", a, st.reads)
	}
	if st.rows[1][1] != "11/5/2026" {
		t.Errorf("cached values changed: %v", st.rows[1])
	}
	// Both have read from Google within the TTL, so both hit the cache.
	if a, b := list(alice, "work"), list(bob, "team"); a[0]["due"] != "2026-11-05" || b[0]["due"] != "11/5/2026" || st.reads != 3 {
		t.Errorf("cache hits: %v %v, %d reads", a, b, st.reads)
	}

	// Alice keeps the shared tab fresh, but Bob's own read has expired, so he must read again.
	clock = clock.Add(45 * time.Second)
	cache.drop(cacheKey("SS", "Tasks"))
	list(alice, "work")
	reads := st.reads
	clock = clock.Add(30 * time.Second)
	if list(bob, "team"); st.reads != reads+1 {
		t.Errorf("bob read the tab Alice refreshed without reading Google: %d reads, want %d", st.reads, reads+1)
	}

	if _, err := alice.Create(ctx, "work", "Tasks", map[string]any{"note": "new"}); err != nil {
		t.Fatal(err)
	}
	reads = st.reads
	if list(bob, "team"); st.reads != reads+1 {
		t.Errorf("bob after alice's write: %d reads, want %d", st.reads, reads+1)
	}
	if alice.cache.lock(cacheKey("SS", "Tasks")) != bob.cache.lock(cacheKey("SS", "Tasks")) {
		t.Error("alice and bob have their own lock for the same tab")
	}
}

// Expired tabs and reads are dropped, even ones no one asks for again.
func TestCacheSweep(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCache(time.Minute)
	c.now = func() time.Time { return clock }
	c.put(1, "SS/Old", &cachedTab{at: clock})
	clock = clock.Add(2 * time.Minute)
	c.put(1, "SS/New", &cachedTab{at: clock})
	if _, ok := c.tabs["SS/Old"]; ok || len(c.tabs) != 1 || len(c.verified) != 1 {
		t.Errorf("after sweep: %v %v", c.tabs, c.verified)
	}
}

// headerStore is a serialStore that can read just the header row.
type headerStore struct {
	*serialStore
	headers int
}

func (f *headerStore) ReadHeader(context.Context, string, string) ([]any, error) {
	f.headers++
	return f.rows[0], nil
}

// Columns reads only the header row and keeps it for a minute even with caching off, per user, and
// a store that can't read just the header reads the whole tab once.
func TestColumnsHeaderCache(t *testing.T) {
	cfg := &db.Settings{Spreadsheets: map[string]db.SheetRule{"shop": {ID: "SS", Tabs: map[string]db.TabRule{
		"Tasks": {Ops: db.AllOps, Key: "sku", Hidden: []string{"cost"}},
	}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	st := &headerStore{serialStore: &serialStore{rows: [][]any{{"sku", "cost", "name"}, {"a", 1.0, "Apple"}}}}
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cache := NewCache(0)
	cache.now = func() time.Time { return clock }
	alice, bob := New(cfg, st, 0), New(cfg, st, 0)
	alice.UseCache(cache, 1)
	bob.UseCache(cache, 2)
	ctx := context.Background()
	columns := func(svc *Service) {
		t.Helper()
		visible, key, err := svc.Columns(ctx, "shop", "Tasks")
		if err != nil || !slices.Equal(visible, []string{"sku", "name"}) || key != "sku" {
			t.Fatalf("columns: %q %q %v", visible, key, err)
		}
	}

	columns(alice)
	columns(alice)
	if st.headers != 1 || st.reads != 0 {
		t.Errorf("alice twice: %d header reads, %d reads, want 1 and 0", st.headers, st.reads)
	}
	columns(bob) // Bob hasn't read it from Google himself
	if st.headers != 2 {
		t.Errorf("bob: %d header reads, want 2", st.headers)
	}
	clock = clock.Add(minHeaderTTL)
	if columns(alice); st.headers != 3 {
		t.Errorf("after a minute: %d header reads, want 3", st.headers)
	}

	plain := New(cfg, st.serialStore, 0)
	plain.UseCache(cache, 3)
	columns(plain)
	columns(plain)
	if st.reads != 1 {
		t.Errorf("without ReadHeader: %d reads, want 1", st.reads)
	}
}
