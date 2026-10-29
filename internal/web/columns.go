package web

import (
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
)

// sampleRows is how many rows under the header the columns page reads to suggest types.
const sampleRows = 50

// columnTypeOptions label the type menu on the columns page, "" (any value) first.
var columnTypeOptions = []struct{ Type, Label string }{
	{"", "Any"},
	{"string", "Text"},
	{"number", "Number"},
	{"integer", "Integer"},
	{"boolean", "True/false"},
	{"date", "Date"},
	{"datetime", "Date and time"},
}

type columnsView struct {
	Sheet, Title string
	Tabs         []tabColumnsView
}

type tabColumnsView struct {
	Name      string
	Err       string // why the tab couldn't be read; it has no Columns then
	Suggested bool   // the types are guesses from its rows, not saved yet
	Key       string // the key column, or "" when the header has none
	KeyRule   string // the key column the tab's rule names, shown when the header lacks it
	Columns   []columnView
	Missing   []columnView // saved columns that row 1 no longer has, renamed or removed in Sheets
}

type columnView struct {
	Name, Type       string
	Required, Hidden bool
	New              bool   // in row 1 but not saved, on a tab whose columns were saved
	Sample           string // the first value under the header, to help pick a type
}

func columnsURL(sheet string) string { return columnsPage + "?sheet=" + url.QueryEscape(sheet) }

// columns shows the columns page for one of u's spreadsheets: each tab's header from Google, with
// its saved types, or types suggested from its rows when it has none yet.
func (d *Dashboard) columns(w http.ResponseWriter, r *http.Request, u user) {
	d.renderColumns(w, r, u, r.FormValue("sheet"), "", http.StatusOK)
}

func (d *Dashboard) renderColumns(w http.ResponseWriter, r *http.Request, u user, sheet, msg string, status int) {
	ctx := r.Context()
	sheets, settings, err := d.DB.Spreadsheets(ctx, u.id)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard", slog.Int64(otelkeys.UserID, u.id), slog.String(otelkeys.Error, err.Error()))
		http.Error(w, "could not load your settings", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(sheets, func(s db.Spreadsheet) bool { return s.Name == sheet })
	if i < 0 {
		http.Redirect(w, r, sheetsPage, http.StatusSeeOther)
		return
	}
	v := columnsView{Sheet: sheet, Title: sheets[i].Title}
	svc, err := d.Tenants.Service(ctx, u.id)
	if err != nil {
		slog.ErrorContext(ctx, "read columns", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Error, err.Error()))
		d.render(w, r, u, view{path: sheetsPage, err: "could not reach Google to read " + sheet + "; sign in again"}, http.StatusBadGateway)
		return
	}
	for _, tab := range sheets[i].Tabs {
		header, rows, err := svc.Peek(ctx, sheet, tab, sampleRows)
		if err != nil {
			slog.ErrorContext(ctx, "read columns", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab),
				slog.String(otelkeys.Error, err.Error()))
			v.Tabs = append(v.Tabs, tabColumnsView{Name: tab, Err: "could not read this tab from Google" + googleDetail(err)})
			continue
		}
		v.Tabs = append(v.Tabs, tabColumns(tab, settings.Spreadsheets[sheet].Tabs[tab], header, rows))
	}
	s := shell{Title: "Columns of " + sheet, Section: sheetsPage, Email: u.email, Admin: u.role.Can(rbac.ManageUsers),
		SheetCount: len(sheets), KeyCount: len(settings.Keys), Error: msg}
	write(w, r, columnsPageView(s, v), status)
}

// tabColumns describes a tab's columns for the columns page: the distinct names in its header,
// with the types rule saved for them, or types guessed from rows for the rest. Saved columns the
// header no longer has are listed as missing, so a renamed one can pass its type on.
func tabColumns(tab string, rule db.TabRule, header []string, rows [][]any) tabColumnsView {
	t := tabColumnsView{Name: tab, Suggested: len(rule.Columns) == 0, KeyRule: rule.KeyColumn()}
	if slices.Contains(header, rule.KeyColumn()) {
		t.Key, t.KeyRule = rule.KeyColumn(), ""
	}
	for i, h := range header {
		if h == "" || slices.ContainsFunc(t.Columns, func(c columnView) bool { return c.Name == h }) {
			continue
		}
		c := columnView{Name: h, Hidden: slices.Contains(rule.Hidden, h)}
		var vals []any
		for _, row := range rows {
			if i < len(row) {
				vals = append(vals, row[i])
				if c.Sample == "" && row[i] != nil {
					c.Sample = fmt.Sprint(row[i])
				}
			}
		}
		col, saved := rule.Columns[h]
		switch {
		case saved:
			c.Type, c.Required = col.Type, col.Required
		case h == t.Key && h == "id":
			c.Type = "string" // the server makes ids, and they are text
		default:
			c.Type = guessType(vals)
		}
		c.New = !saved && !t.Suggested
		t.Columns = append(t.Columns, c)
	}
	for _, name := range slices.Sorted(maps.Keys(rule.Columns)) {
		if !slices.Contains(header, name) {
			col := rule.Columns[name]
			t.Missing = append(t.Missing, columnView{Name: name, Type: col.Type, Required: col.Required})
		}
	}
	return t
}

// typeLabel returns how the columns page names a column type.
func typeLabel(typ string) string {
	for _, o := range columnTypeOptions {
		if o.Type == typ {
			return o.Label
		}
	}
	return typ
}

// guessType returns the column type every non-blank value in vals fits, or "" (any) when there are
// none. Values of different types make it "string". Sheets returns numbers and checkboxes typed, but
// numbers can also be stored as text, and dates come back as text.
func guessType(vals []any) string {
	typ := ""
	for _, v := range vals {
		var t string
		switch v := v.(type) {
		case nil:
			continue
		case bool:
			t = "boolean"
		case float64:
			t = "integer"
			if v != math.Trunc(v) {
				t = "number"
			}
		case string:
			if strings.TrimSpace(v) == "" {
				continue
			}
			t = "string"
			if f, err := strconv.ParseFloat(v, 64); err == nil && !leadingZero(v) {
				t = "integer" // a number stored as text; the API reads and writes it as a number
				if f != math.Trunc(f) {
					t = "number"
				}
			} else if _, err := time.Parse(time.DateOnly, v); err == nil {
				t = "date"
			} else if _, err := time.Parse(time.RFC3339, v); err == nil {
				t = "datetime"
			}
		default:
			return ""
		}
		switch {
		case typ == "" || typ == t:
			typ = t
		case typ == "integer" && t == "number", typ == "number" && t == "integer":
			typ = "number"
		default:
			return "string" // mixed values, all shown as text
		}
	}
	return typ
}

// leadingZero reports whether s, a number as text, starts with a 0 that a number would drop, as a
// ZIP code or phone number might. Such a column is text.
func leadingZero(s string) bool {
	s = strings.TrimPrefix(s, "-")
	return len(s) > 1 && s[0] == '0' && s[1] != '.'
}

// tabForm is one tab as the columns page or the new spreadsheet page posts it.
type tabForm struct {
	Name    string
	Key     string // the key column; "" for none, which on the new spreadsheet page means an id column
	Columns []columnForm
}

type columnForm struct {
	Name, Type       string
	Required, Hidden bool
}

// parseTabs reads the tabs a form lists. Inputs are numbered, since tab and column names can hold
// any character: t<i> is the i-th tab's name and t<i>key its key column; t<i>c<j> is the name of
// its j-th column, and t<i>c<j>type, ...required and ...hidden the rest of it. Names are trimmed,
// and a column without one is skipped, as a blank row on the new spreadsheet page is.
func parseTabs(form url.Values) []tabForm {
	var tabs []tabForm
	for i := 0; ; i++ {
		ti := "t" + strconv.Itoa(i)
		if _, ok := form[ti]; !ok {
			return tabs
		}
		t := tabForm{Name: strings.TrimSpace(form.Get(ti)), Key: strings.TrimSpace(form.Get(ti + "key"))}
		for j := 0; ; j++ {
			cj := ti + "c" + strconv.Itoa(j)
			if _, ok := form[cj]; !ok {
				break
			}
			if name := strings.TrimSpace(form.Get(cj)); name != "" {
				t.Columns = append(t.Columns, columnForm{Name: name, Type: form.Get(cj + "type"),
					Required: form.Get(cj+"required") != "", Hidden: form.Get(cj+"hidden") != ""})
			}
		}
		tabs = append(tabs, t)
	}
}

// rule returns the tab's key column, hidden columns and column types, or why they don't fit together.
func (t tabForm) rule() (db.TabRule, error) {
	rule := db.TabRule{Key: t.Key, Columns: map[string]db.Column{}}
	for _, c := range t.Columns {
		if _, dup := rule.Columns[c.Name]; dup {
			return rule, fmt.Errorf("%s: column %q is listed twice", t.Name, c.Name)
		}
		rule.Columns[c.Name] = db.Column{Type: c.Type, Required: c.Required}
		if c.Hidden {
			rule.Hidden = append(rule.Hidden, c.Name)
		}
	}
	if _, ok := rule.Columns[t.Key]; t.Key != "" && !ok {
		return rule, fmt.Errorf("%s: the key column %q is not one of its columns", t.Name, t.Key)
	}
	if slices.Contains(rule.Hidden, t.Key) {
		return rule, fmt.Errorf("%s: the key column %s can't be hidden", t.Name, t.Key)
	}
	return rule, nil
}

// saveColumns saves the columns page's form: for each tab, its key column, hidden columns and
// each column's type and whether it's required.
func (d *Dashboard) saveColumns(w http.ResponseWriter, r *http.Request, u user) {
	sheet := r.FormValue("sheet")
	fail := func(msg string) { d.renderColumns(w, r, u, sheet, msg, http.StatusBadRequest) }
	tabs := map[string]db.TabRule{}
	for _, t := range parseTabs(r.PostForm) {
		rule, err := t.rule()
		if err != nil {
			fail(err.Error())
			return
		}
		tabs[t.Name] = rule
	}
	err := d.DB.SaveColumns(r.Context(), u.id, sheet, tabs)
	if err != nil {
		fail(userMessage(err))
		return
	}
	for tab, rule := range tabs {
		audit(r.Context(), u, "tab.columns", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab),
			slog.String(otelkeys.KeyColumn, cmp.Or(rule.Key, "id")), slog.String(otelkeys.Hidden, strings.Join(rule.Hidden, ",")),
			slog.String(otelkeys.Columns, columnsAttr(rule.Columns)))
	}
	d.change(w, r, u, sheetsPage+"#sheet-"+sheet, nil)
}

// columnsAttr lists columns as "name:type" for the audit log, with ! after a required one.
func columnsAttr(cols map[string]db.Column) string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(cols)) {
		s := name + ":" + cmp.Or(cols[name].Type, "any")
		if cols[name].Required {
			s += "!"
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}
