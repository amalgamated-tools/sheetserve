package service

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func BadRequest(format string, a ...any) error {
	return &APIError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

var errNotFound = &APIError{http.StatusNotFound, "row not found"}

// Service maps tabs to rows of objects keyed by the header row. A row is found by the value in
// the tab's key column; a tab whose header lacks that column can only be listed.
type Service struct {
	settings *db.Settings
	store    store.Store
	cache    *Cache
	user     int64 // whose credentials store uses, for the cache
}

type snapshot struct {
	header []string
	rows   [][]any
	key    int // index of the key column in header, or -1 when the tab has none
}

// New returns a Service with a cache of its own that keeps tabs for cacheTTL. Tenants share one
// with UseCache.
func New(settings *db.Settings, store store.Store, cacheTTL time.Duration) *Service {
	return &Service{settings: settings, store: store, cache: NewCache(cacheTTL)}
}

// UseCache makes s read tabs through, and lock them in, c, as user.
func (s *Service) UseCache(c *Cache, user int64) { s.cache, s.user = c, user }

// Room returns how much Google quota the caller on ctx has left, or nil when its store has no
// limits.
func (s *Service) Room(ctx context.Context) []store.Room {
	if r, ok := s.store.(store.Roomer); ok {
		return r.Room(ctx)
	}
	return nil
}

func (s *Service) SharesCache(other *Service) bool { return s.cache == other.cache }

func (s *Service) Tabs(ctx context.Context, spreadsheetID string) ([]string, error) {
	return s.store.Tabs(ctx, spreadsheetID)
}

// load returns the tab with this user's settings applied: its key column, and dates as ISO 8601.
// It reads through the cache unless fresh is set.
//
// TODO: the cache and locks are in-process; a second replica needs Redis and a distributed lock.
func (s *Service) load(ctx context.Context, sheet, tab string, fresh bool) (snapshot, error) {
	t, cached, err := s.read(ctx, sheet, tab, fresh)
	if err != nil {
		return snapshot{}, err
	}
	// A date column this tab's cached values lack serials for is read now, so read the values again
	// with it, or dates could land on rows that moved since the values were read.
	if _, missing := s.cache.serials(t, s.dateColumns(sheet, tab, t.vals)); cached && len(missing) > 0 {
		if t, err = s.fetch(ctx, sheet, tab, true); err != nil {
			return snapshot{}, err
		}
	}
	snap := snapshot{header: headerOf(t.vals)}
	if len(t.vals) > 0 {
		snap.rows = t.vals[1:]
	}
	snap.key = slices.Index(snap.header, s.keyColumn(sheet, tab))
	if err := s.readDates(ctx, sheet, tab, &snap, t); err != nil {
		return snapshot{}, err
	}
	return snap, nil
}

// read returns the tab's values, and whether they came from the cache. Unless fresh is set, it
// reads through the cache, and caches what it reads from Google.
func (s *Service) read(ctx context.Context, sheet, tab string, fresh bool) (*cachedTab, bool, error) {
	if !fresh {
		if t, ok := s.cache.get(s.user, cacheKey(s.settings.Spreadsheets[sheet].ID, tab)); ok {
			slog.DebugContext(ctx, "cache hit", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab),
				slog.Int64(otelkeys.AgeMs, s.cache.now().Sub(t.at).Milliseconds()))
			return t, true, nil
		}
		if s.cache.ttl > 0 {
			slog.DebugContext(ctx, "cache miss", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab))
		}
	}
	t, err := s.fetch(ctx, sheet, tab, !fresh)
	return t, false, err
}

// fetch reads the tab's values from Google with this user's credentials, and caches them if keep.
func (s *Service) fetch(ctx context.Context, sheet, tab string, keep bool) (*cachedTab, error) {
	id := s.settings.Spreadsheets[sheet].ID
	vals, err := s.store.Read(ctx, id, tab)
	if err != nil {
		return nil, err
	}
	t := &cachedTab{vals: vals, serials: map[int][]any{}, at: s.cache.now()}
	if keep {
		s.cache.put(s.user, cacheKey(id, tab), t)
	}
	return t, nil
}

// dateColumns returns the indexes of the date and datetime columns in vals' header row, when the
// store can read them as serial numbers.
func (s *Service) dateColumns(sheet, tab string, vals [][]any) []int {
	if _, ok := s.store.(store.SerialReader); !ok || len(vals) == 0 {
		return nil
	}
	cols := s.columns(sheet, tab)
	var idx []int
	for i, h := range vals[0] {
		if typ := cols[fmt.Sprint(h)].Type; typ == "date" || typ == "datetime" {
			idx = append(idx, i)
		}
	}
	return idx
}

// readDates replaces the cells of snap's date and datetime columns that hold dates with ISO 8601
// text, in a copy of the rows, since t may be shared. Sheets shows a date in the spreadsheet's
// locale, so those columns are read again as serial numbers, which t keeps. Text in them stays as
// it is.
func (s *Service) readDates(ctx context.Context, sheet, tab string, snap *snapshot, t *cachedTab) error {
	idx := s.dateColumns(sheet, tab, t.vals)
	sr, ok := s.store.(store.SerialReader)
	if !ok || len(idx) == 0 {
		return nil
	}
	cols := s.columns(sheet, tab)
	serials, missing := s.cache.serials(t, idx)
	if len(missing) > 0 {
		read, err := sr.ReadSerial(ctx, s.settings.Spreadsheets[sheet].ID, tab, missing)
		if err != nil {
			return err
		}
		got := map[int][]any{}
		for k, c := range missing {
			if k < len(read) {
				got[c], serials[c] = read[k], read[k]
			}
		}
		s.cache.addSerials(t, got)
	}
	rows := make([][]any, len(snap.rows))
	for i, r := range snap.rows {
		rows[i] = slices.Clone(r)
	}
	for _, c := range idx {
		for r, v := range serials[c] {
			f, isNum := v.(float64)
			if r == 0 || !isNum || r-1 >= len(rows) || c >= len(rows[r-1]) {
				continue // the header, text, or a cell the first read didn't have
			}
			rows[r-1][c] = fromSerial(f, cols[snap.header[c]].Type == "datetime")
		}
	}
	snap.rows = rows
	return nil
}

// lockTab serializes writes to one tab, across every user who added its spreadsheet, and drops
// the tab from the cache once the write is done.
func (s *Service) lockTab(ctx context.Context, sheet, tab string) func() {
	k := cacheKey(s.settings.Spreadsheets[sheet].ID, tab)
	m := s.cache.lock(k)
	m.Lock()
	return func() {
		cached := s.cache.drop(k)
		m.Unlock()
		if cached {
			slog.DebugContext(ctx, "cache invalidate", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab))
		}
	}
}

func (s *Service) hidden(sheet, tab string) []string {
	return s.settings.Spreadsheets[sheet].Tabs[tab].Hidden
}

func (s *Service) keyColumn(sheet, tab string) string {
	return s.settings.Spreadsheets[sheet].Tabs[tab].KeyColumn()
}

// Header returns the tab's header row, hidden columns included.
func (s *Service) Header(ctx context.Context, sheet, tab string) ([]string, error) {
	snap, err := s.load(ctx, sheet, tab, false)
	return snap.header, err
}

// Peek reads the tab fresh from Google and returns its header row and up to n rows under it,
// hidden columns included, for the dashboard to show its owner.
func (s *Service) Peek(ctx context.Context, sheet, tab string, n int) ([]string, [][]any, error) {
	snap, err := s.load(ctx, sheet, tab, true)
	return snap.header, snap.rows[:min(n, len(snap.rows))], err
}

// headerOf returns the header row of a tab's values, as text.
func headerOf(vals [][]any) []string {
	if len(vals) == 0 {
		return nil
	}
	header := make([]string, len(vals[0]))
	for i, h := range vals[0] {
		header[i] = fmt.Sprint(h)
	}
	return header
}

// header returns the tab's header row, from a cached read of the tab or of its header if this user
// has one, or else read from Google on its own when the store can.
func (s *Service) header(ctx context.Context, sheet, tab string) ([]string, error) {
	id := s.settings.Spreadsheets[sheet].ID
	k := cacheKey(id, tab)
	if t, ok := s.cache.get(s.user, k); ok {
		return headerOf(t.vals), nil
	}
	if h, ok := s.cache.header(s.user, k); ok {
		return h, nil
	}
	hr, ok := s.store.(store.HeaderReader)
	if !ok {
		t, _, err := s.read(ctx, sheet, tab, false) // caches the header
		if err != nil {
			return nil, err
		}
		return headerOf(t.vals), nil
	}
	row, err := hr.ReadHeader(ctx, id, tab)
	if err != nil {
		return nil, err
	}
	h := headerOf([][]any{row})
	s.cache.putHeader(s.user, k, h)
	return h, nil
}

// Columns returns the columns of the tab a caller may see, in sheet order, and its key column, or
// "" when its header lacks one and the tab can only be listed. It needs only the header row.
func (s *Service) Columns(ctx context.Context, sheet, tab string) (visible []string, key string, err error) {
	header, err := s.header(ctx, sheet, tab)
	if err != nil {
		return nil, "", err
	}
	if k := s.keyColumn(sheet, tab); slices.Contains(header, k) {
		key = k
	}
	return s.visibleColumns(snapshot{header: header}, sheet, tab), key, nil
}

// visibleColumns returns header columns a caller may see, in sheet order.
func (s *Service) visibleColumns(snap snapshot, sheet, tab string) []string {
	hidden := s.hidden(sheet, tab)
	return slices.DeleteFunc(slices.Clone(snap.header), func(h string) bool {
		return h == "" || slices.Contains(hidden, h)
	})
}

func (s *Service) columns(sheet, tab string) map[string]db.Column {
	return s.settings.Spreadsheets[sheet].Tabs[tab].Columns
}

func (s *Service) toObject(snap snapshot, row []any, sheet, tab string) map[string]any {
	hidden, cols := s.hidden(sheet, tab), s.columns(sheet, tab)
	m := map[string]any{}
	for i, h := range snap.header {
		if h == "" || slices.Contains(hidden, h) {
			continue
		}
		var v any
		if i < len(row) {
			v = row[i]
		}
		m[h] = readValue(cols[h].Type, v)
	}
	return m
}

// find returns the index in snap.rows of the one row whose key column holds val.
func (s *Service) find(snap snapshot, sheet, tab, val string) (int, error) {
	if snap.key < 0 {
		return 0, s.noKey(sheet, tab)
	}
	found := -1
	for i, r := range snap.rows {
		if snap.key < len(r) && fmt.Sprint(r[snap.key]) == val {
			if found >= 0 {
				return 0, &APIError{http.StatusConflict, fmt.Sprintf("more than one row has %s %q; make the key column unique", snap.header[snap.key], val)}
			}
			found = i
		}
	}
	if found < 0 {
		return 0, errNotFound
	}
	return found, nil
}

// noKey refuses an operation on one row of a tab that has no key column.
func (s *Service) noKey(sheet, tab string) error {
	return &APIError{http.StatusForbidden, fmt.Sprintf("tab %s/%s has no %q column, so it can only be listed", sheet, tab, s.keyColumn(sheet, tab))}
}

// filterOps are the operators a filter key can carry as a suffix, as in "qty[gte]". No suffix means eq.
var filterOps = []string{"eq", "ne", "gt", "gte", "lt", "lte", "contains"}

func FilterOperators() []string { return slices.Clone(filterOps) }

type cond struct {
	col int
	typ string // the column's type
	op  string
	val string
}

// List returns rows matching every filter, sorted by sort ("col" or "-col" for descending), then
// paged, and how many rows matched before paging.
// Filter keys are "col" or "col[op]". An exact column name always wins over a sort prefix or filter
// suffix, so columns named "-x" or "x[ne]" stay reachable.
func (s *Service) List(ctx context.Context, sheet, tab string, filter map[string]string, sort string, limit, offset int) (_ []map[string]any, total int, err error) {
	if limit < 0 {
		return nil, 0, BadRequest("invalid limit")
	}
	if offset < 0 {
		return nil, 0, BadRequest("invalid offset")
	}
	snap, err := s.load(ctx, sheet, tab, false)
	if err != nil {
		return nil, 0, err
	}
	visible := s.visibleColumns(snap, sheet, tab)
	// Filtering or sorting on a hidden column would leak its values.
	column := func(name string) (int, error) {
		if !slices.Contains(visible, name) {
			return 0, BadRequest("unknown column %q", name)
		}
		return slices.Index(snap.header, name), nil
	}
	var conds []cond
	for k, v := range filter {
		name, op := k, "eq"
		if j := strings.LastIndex(k, "["); j > 0 && strings.HasSuffix(k, "]") && !slices.Contains(visible, k) {
			name, op = k[:j], k[j+1:len(k)-1]
			if !slices.Contains(filterOps, op) {
				return nil, 0, BadRequest("unknown operator %q (want one of %v)", op, filterOps)
			}
		}
		i, err := column(name)
		if err != nil {
			return nil, 0, err
		}
		col := s.columns(sheet, tab)[name]
		if v, err = filterValue(col, k, op, v); err != nil {
			return nil, 0, err
		}
		conds = append(conds, cond{i, col.Type, op, v})
	}
	var matched [][]any
	for _, row := range snap.rows {
		if !slices.ContainsFunc(conds, func(c cond) bool { return !c.match(cell(row, c.col)) }) {
			matched = append(matched, row)
		}
	}
	if sort != "" {
		name, desc := sort, false
		if !slices.Contains(visible, sort) {
			name, desc = strings.CutPrefix(sort, "-")
		}
		i, err := column(name)
		if err != nil {
			return nil, 0, err
		}
		slices.SortStableFunc(matched, func(a, b []any) int {
			c := compare(cell(a, i), cell(b, i))
			if desc {
				return -c
			}
			return c
		})
	}
	total = len(matched)
	matched = matched[min(offset, len(matched)):]
	if limit > 0 {
		matched = matched[:min(limit, len(matched))]
	}
	out := []map[string]any{}
	for _, row := range matched {
		out = append(out, s.toObject(snap, row, sheet, tab))
	}
	return out, total, nil
}

func cell(row []any, i int) string {
	if i < len(row) {
		return fmt.Sprint(row[i])
	}
	return ""
}

func (c cond) match(v string) bool {
	if c.typ != "" && v == "" && (c.op == "gt" || c.op == "gte" || c.op == "lt" || c.op == "lte") {
		return false // an empty cell in a typed column has no value to compare
	}
	switch c.typ {
	case "number", "integer":
		if c.op == "eq" || c.op == "ne" { // 2 matches 2.0
			x, errX := strconv.ParseFloat(v, 64)
			y, _ := strconv.ParseFloat(c.val, 64)
			return (errX == nil && x == y) == (c.op == "eq")
		}
	case "boolean":
		v = strings.ToLower(v) // a checkbox reads as true, text as TRUE
	}
	switch c.op {
	case "ne":
		return v != c.val
	case "gt":
		return compare(v, c.val) > 0
	case "gte":
		return compare(v, c.val) >= 0
	case "lt":
		return compare(v, c.val) < 0
	case "lte":
		return compare(v, c.val) <= 0
	case "contains":
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.val))
	}
	return v == c.val
}

// compare orders numbers numerically before all non-numbers, which order as strings (so ISO dates
// work). Ranking numbers first keeps the order consistent on mixed columns.
func compare(a, b string) int {
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	switch {
	case errA == nil && errB == nil:
		return cmp.Compare(x, y)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func (s *Service) Get(ctx context.Context, sheet, tab, id string) (map[string]any, error) {
	snap, err := s.load(ctx, sheet, tab, false)
	if err != nil {
		return nil, err
	}
	i, err := s.find(snap, sheet, tab, id)
	if err != nil {
		return nil, err
	}
	return s.toObject(snap, snap.rows[i], sheet, tab), nil
}

// checkFields rejects writes to unknown or hidden columns, and to the key column unless creating
// a row on a tab whose key isn't "id". The server assigns ids, and a row's key never changes. It
// also checks each value against its column's type, and that a new row fills every required column
// that has no default.
func (s *Service) checkFields(snap snapshot, sheet, tab string, fields map[string]any, creating bool) error {
	visible := s.visibleColumns(snap, sheet, tab)
	key := snap.header[snap.key]
	cols := s.columns(sheet, tab)
	for _, f := range slices.Sorted(maps.Keys(fields)) {
		switch {
		case f == key && key == "id":
			return BadRequest("id is assigned by the server")
		case f == key && !creating:
			return BadRequest("%s is the tab's key and can't be changed", key)
		}
		if !slices.Contains(visible, f) {
			return BadRequest("unknown column %q", f)
		}
		if err := checkValue(f, cols[f], fields[f]); err != nil {
			return err
		}
	}
	if creating {
		for _, c := range visible {
			if _, sent := fields[c]; !sent && cols[c].Required && cols[c].Default == "" && (c != key || key != "id") {
				return BadRequest("%s is required", c)
			}
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, sheet, tab string, fields map[string]any) (map[string]any, error) {
	defer s.lockTab(ctx, sheet, tab)()
	snap, err := s.load(ctx, sheet, tab, true)
	if err != nil {
		return nil, err
	}
	if snap.key < 0 {
		return nil, s.noKey(sheet, tab)
	}
	if err := s.checkFields(snap, sheet, tab, fields, true); err != nil {
		return nil, err
	}
	row, cols := make([]any, len(snap.header)), s.columns(sheet, tab)
	for i, h := range snap.header {
		v, sent := fields[h]
		if d, ok := DefaultValue(cols[h]); !sent && ok && i != snap.key {
			v = d
		}
		row[i] = cellValue(normalize(cols[h].Type, v))
	}
	if key := snap.header[snap.key]; key == "id" {
		row[snap.key] = rand.Text()
	} else if val := fmt.Sprint(row[snap.key]); val == "" {
		return nil, BadRequest("%s is the tab's key and is required", key)
	} else {
		switch _, err := s.find(snap, sheet, tab, val); {
		case err == nil:
			return nil, &APIError{http.StatusConflict, fmt.Sprintf("a row with %s %q already exists", key, val)}
		case err != errNotFound:
			return nil, err
		}
	}
	out := make([]any, len(row))
	for i, h := range snap.header {
		out[i] = toStore(cols[h].Type, row[i])
	}
	if err := s.store.Append(ctx, s.settings.Spreadsheets[sheet].ID, tab, out); err != nil {
		return nil, err
	}
	return s.toObject(snap, row, sheet, tab), nil
}

// locateTries is how many times locate reads a tab again when the row it found moves before it can
// be written.
const locateTries = 3

// locate reads the tab fresh and finds the row whose key is id, then reads that row's key cell back
// from the sheet just before the caller writes to it. Someone inserting or deleting rows by hand in
// the meantime moves rows, so when the cell no longer holds id, it reads the tab again. The caller
// must hold the tab's lock. Only the moments between that check and the write stay unguarded. check,
// if set, vets each read of the tab before the row is looked for.
func (s *Service) locate(ctx context.Context, sheet, tab, id string, check func(snapshot) error) (snapshot, int, error) {
	cr, canCheck := s.store.(store.CellReader)
	for range locateTries {
		snap, err := s.load(ctx, sheet, tab, true)
		if err != nil {
			return snapshot{}, 0, err
		}
		if check != nil {
			if err := check(snap); err != nil {
				return snapshot{}, 0, err
			}
		}
		i, err := s.find(snap, sheet, tab, id)
		if err != nil || !canCheck {
			return snap, i, err
		}
		v, err := cr.ReadCell(ctx, s.settings.Spreadsheets[sheet].ID, tab, i+2, snap.key)
		if err != nil {
			return snapshot{}, 0, err
		}
		if f, isNum := v.(float64); isNum {
			if t := s.columns(sheet, tab)[snap.header[snap.key]].Type; t == "date" || t == "datetime" {
				v = fromSerial(f, t == "datetime")
			}
		}
		if v != nil && fmt.Sprint(v) == id {
			return snap, i, nil
		}
		slog.WarnContext(ctx, "row moved before write", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab))
	}
	return snapshot{}, 0, &APIError{http.StatusConflict, "rows in " + sheet + "/" + tab + " kept moving while writing; try again"}
}

func (s *Service) Update(ctx context.Context, sheet, tab, id string, fields map[string]any) (map[string]any, error) {
	if len(fields) == 0 {
		return nil, BadRequest("no fields to update")
	}
	defer s.lockTab(ctx, sheet, tab)()
	snap, i, err := s.locate(ctx, sheet, tab, id, func(snap snapshot) error {
		if snap.key < 0 {
			return s.noKey(sheet, tab)
		}
		return s.checkFields(snap, sheet, tab, fields, false)
	})
	if err != nil {
		return nil, err
	}
	row := make([]any, len(snap.header))
	copy(row, snap.rows[i])
	cells, cols := map[int]any{}, s.columns(sheet, tab)
	for f, v := range fields {
		c := slices.Index(snap.header, f)
		v = cellValue(normalize(cols[f].Type, v))
		cells[c], row[c] = toStore(cols[f].Type, v), v
	}
	if err := s.store.SetCells(ctx, s.settings.Spreadsheets[sheet].ID, tab, i+2, cells); err != nil {
		return nil, err
	}
	return s.toObject(snap, row, sheet, tab), nil
}

func (s *Service) Delete(ctx context.Context, sheet, tab, id string) error {
	defer s.lockTab(ctx, sheet, tab)()
	_, i, err := s.locate(ctx, sheet, tab, id, nil)
	if err != nil {
		return err
	}
	return s.store.DeleteRow(ctx, s.settings.Spreadsheets[sheet].ID, tab, i+2)
}

// cellValue maps JSON null to an empty cell; Sheets skips nil values instead of clearing them.
func cellValue(v any) any {
	if v == nil {
		return ""
	}
	return v
}
