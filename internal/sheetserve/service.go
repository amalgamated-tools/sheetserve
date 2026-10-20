package sheetserve

import (
	"cmp"
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func badRequest(format string, a ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

var errNotFound = &apiError{http.StatusNotFound, "row not found"}

// Service maps tabs to rows of objects keyed by the header row. Every tab needs an "id" column.
type Service struct {
	cfg   *Config
	store Store

	mu    sync.Mutex
	cache map[string]snapshot
	locks *sync.Map // "sheet/tab" -> *sync.Mutex
}

type snapshot struct {
	header []string
	rows   [][]any
	at     time.Time
}

func newService(cfg *Config, store Store) *Service {
	return &Service{cfg: cfg, store: store, cache: map[string]snapshot{}, locks: &sync.Map{}}
}

// ponytail: in-process cache and locks; a second replica needs Redis + a distributed lock.
func (s *Service) load(ctx context.Context, sheet, tab string, fresh bool) (snapshot, error) {
	k := sheet + "/" + tab
	if !fresh && s.cfg.CacheTTL > 0 {
		s.mu.Lock()
		snap, ok := s.cache[k]
		s.mu.Unlock()
		if ok && time.Since(snap.at) < s.cfg.CacheTTL {
			return snap, nil
		}
	}
	vals, err := s.store.Read(ctx, s.cfg.Spreadsheets[sheet].ID, tab)
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{at: time.Now()}
	if len(vals) > 0 {
		for _, h := range vals[0] {
			snap.header = append(snap.header, fmt.Sprint(h))
		}
		snap.rows = vals[1:]
	}
	if !slices.Contains(snap.header, "id") {
		return snapshot{}, fmt.Errorf("tab %s has no id column", k)
	}
	if s.cfg.CacheTTL > 0 {
		s.mu.Lock()
		s.cache[k] = snap
		s.mu.Unlock()
	}
	return snap, nil
}

// lockTab serializes writes to one tab and drops its cache entry once the write is done.
func (s *Service) lockTab(sheet, tab string) func() {
	k := sheet + "/" + tab
	m, _ := s.locks.LoadOrStore(k, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return func() {
		s.mu.Lock()
		delete(s.cache, k)
		s.mu.Unlock()
		m.(*sync.Mutex).Unlock()
	}
}

func (s *Service) hidden(sheet, tab string) []string {
	return s.cfg.Spreadsheets[sheet].Tabs[tab].Hidden
}

// visibleColumns returns header columns a caller may see, in sheet order.
func (s *Service) visibleColumns(snap snapshot, sheet, tab string) []string {
	hidden := s.hidden(sheet, tab)
	return slices.DeleteFunc(slices.Clone(snap.header), func(h string) bool {
		return h == "" || slices.Contains(hidden, h)
	})
}

func (s *Service) toObject(snap snapshot, row []any, sheet, tab string) map[string]any {
	hidden := s.hidden(sheet, tab)
	m := map[string]any{}
	for i, h := range snap.header {
		if h == "" || slices.Contains(hidden, h) {
			continue
		}
		var v any
		if i < len(row) {
			v = row[i]
		}
		m[h] = v
	}
	return m
}

func findID(snap snapshot, id string) int {
	col := slices.Index(snap.header, "id")
	return slices.IndexFunc(snap.rows, func(r []any) bool {
		return col < len(r) && fmt.Sprint(r[col]) == id
	})
}

// filterOps are the operators a filter key can carry as a suffix, as in "qty[gte]". No suffix means eq.
var filterOps = []string{"eq", "ne", "gt", "gte", "lt", "lte", "contains"}

type cond struct {
	col int
	op  string
	val string
}

// List returns rows matching every filter, sorted by sort ("col" or "-col" for descending), then paged.
// Filter keys are "col" or "col[op]". An exact column name always wins over a sort prefix or filter
// suffix, so columns named "-x" or "x[ne]" stay reachable.
func (s *Service) List(ctx context.Context, sheet, tab string, filter map[string]string, sort string, limit, offset int) ([]map[string]any, error) {
	if limit < 0 {
		return nil, badRequest("invalid limit")
	}
	if offset < 0 {
		return nil, badRequest("invalid offset")
	}
	snap, err := s.load(ctx, sheet, tab, false)
	if err != nil {
		return nil, err
	}
	visible := s.visibleColumns(snap, sheet, tab)
	// Filtering or sorting on a hidden column would leak its values.
	column := func(name string) (int, error) {
		if !slices.Contains(visible, name) {
			return 0, badRequest("unknown column %q", name)
		}
		return slices.Index(snap.header, name), nil
	}
	var conds []cond
	for k, v := range filter {
		name, op := k, "eq"
		if j := strings.LastIndex(k, "["); j > 0 && strings.HasSuffix(k, "]") && !slices.Contains(visible, k) {
			name, op = k[:j], k[j+1:len(k)-1]
			if !slices.Contains(filterOps, op) {
				return nil, badRequest("unknown operator %q (want one of %v)", op, filterOps)
			}
		}
		i, err := column(name)
		if err != nil {
			return nil, err
		}
		conds = append(conds, cond{i, op, v})
	}
	var matched [][]any
	for _, row := range snap.rows {
		if sort == "" && limit > 0 && len(matched) == offset+limit {
			break // unsorted: the page is already complete
		}
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
			return nil, err
		}
		slices.SortStableFunc(matched, func(a, b []any) int {
			c := compare(cell(a, i), cell(b, i))
			if desc {
				return -c
			}
			return c
		})
	}
	matched = matched[min(offset, len(matched)):]
	if limit > 0 {
		matched = matched[:min(limit, len(matched))]
	}
	out := []map[string]any{}
	for _, row := range matched {
		out = append(out, s.toObject(snap, row, sheet, tab))
	}
	return out, nil
}

func cell(row []any, i int) string {
	if i < len(row) {
		return fmt.Sprint(row[i])
	}
	return ""
}

func (c cond) match(v string) bool {
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
	i := findID(snap, id)
	if i < 0 {
		return nil, errNotFound
	}
	return s.toObject(snap, snap.rows[i], sheet, tab), nil
}

// checkFields rejects writes to unknown, hidden, or id columns.
func (s *Service) checkFields(snap snapshot, sheet, tab string, fields map[string]any) error {
	visible := s.visibleColumns(snap, sheet, tab)
	for _, f := range slices.Sorted(maps.Keys(fields)) {
		if f == "id" {
			return badRequest("id is assigned by the server")
		}
		if !slices.Contains(visible, f) {
			return badRequest("unknown column %q", f)
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, sheet, tab string, fields map[string]any) (map[string]any, error) {
	defer s.lockTab(sheet, tab)()
	snap, err := s.load(ctx, sheet, tab, true)
	if err != nil {
		return nil, err
	}
	if err := s.checkFields(snap, sheet, tab, fields); err != nil {
		return nil, err
	}
	row := make([]any, len(snap.header))
	for i, h := range snap.header {
		row[i] = cellValue(fields[h])
	}
	row[slices.Index(snap.header, "id")] = rand.Text()
	if err := s.store.Append(ctx, s.cfg.Spreadsheets[sheet].ID, tab, row); err != nil {
		return nil, err
	}
	return s.toObject(snap, row, sheet, tab), nil
}

// ponytail: rows are located by id just before writing; a human inserting/deleting rows in the
// sheet during that window can shift the target row. Fine for API-owned tabs.
func (s *Service) Update(ctx context.Context, sheet, tab, id string, fields map[string]any) (map[string]any, error) {
	if len(fields) == 0 {
		return nil, badRequest("no fields to update")
	}
	defer s.lockTab(sheet, tab)()
	snap, err := s.load(ctx, sheet, tab, true)
	if err != nil {
		return nil, err
	}
	if err := s.checkFields(snap, sheet, tab, fields); err != nil {
		return nil, err
	}
	i := findID(snap, id)
	if i < 0 {
		return nil, errNotFound
	}
	row := make([]any, len(snap.header))
	copy(row, snap.rows[i])
	cells := map[int]any{}
	for f, v := range fields {
		c := slices.Index(snap.header, f)
		cells[c], row[c] = cellValue(v), cellValue(v)
	}
	if err := s.store.SetCells(ctx, s.cfg.Spreadsheets[sheet].ID, tab, i+2, cells); err != nil {
		return nil, err
	}
	return s.toObject(snap, row, sheet, tab), nil
}

func (s *Service) Delete(ctx context.Context, sheet, tab, id string) error {
	defer s.lockTab(sheet, tab)()
	snap, err := s.load(ctx, sheet, tab, true)
	if err != nil {
		return err
	}
	i := findID(snap, id)
	if i < 0 {
		return errNotFound
	}
	return s.store.DeleteRow(ctx, s.cfg.Spreadsheets[sheet].ID, tab, i+2)
}

// cellValue maps JSON null to an empty cell; Sheets skips nil values instead of clearing them.
func cellValue(v any) any {
	if v == nil {
		return ""
	}
	return v
}
