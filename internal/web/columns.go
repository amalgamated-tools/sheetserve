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
}

type columnView struct {
	Name, Type       string
	Required, Hidden bool
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
// with the types rule saved for them, or types guessed from rows if rule has none.
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
		if col, ok := rule.Columns[h]; ok {
			c.Type, c.Required = col.Type, col.Required
		} else if t.Suggested && h != t.Key {
			c.Type = guessType(vals)
		} else if h == t.Key && h == "id" {
			c.Type = "string" // the server makes ids, and they are text
		}
		t.Columns = append(t.Columns, c)
	}
	return t
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

// saveColumns saves the columns page's form: for each tab, its key column, hidden columns and
// each column's type and whether it's required. Inputs are numbered, t<i>... for the i-th tab
// and t<i>c<j>... for its j-th column, since tab and column names can hold any character.
func (d *Dashboard) saveColumns(w http.ResponseWriter, r *http.Request, u user) {
	sheet := r.FormValue("sheet")
	fail := func(msg string) { d.renderColumns(w, r, u, sheet, msg, http.StatusBadRequest) }
	tabs := map[string]db.TabRule{}
	for i := 0; ; i++ {
		ti := "t" + strconv.Itoa(i)
		tab, ok := r.PostForm[ti]
		if !ok {
			break
		}
		rule := db.TabRule{Key: r.FormValue(ti + "key"), Columns: map[string]db.Column{}}
		for j := 0; ; j++ {
			cj := ti + "c" + strconv.Itoa(j)
			name := r.FormValue(cj)
			if name == "" {
				break
			}
			rule.Columns[name] = db.Column{Type: r.FormValue(cj + "type"), Required: r.FormValue(cj+"required") != ""}
			if r.FormValue(cj+"hidden") != "" {
				rule.Hidden = append(rule.Hidden, name)
			}
		}
		if _, ok := rule.Columns[rule.Key]; rule.Key != "" && !ok {
			fail(fmt.Sprintf("%s: the key column %q is not one of its columns", tab[0], rule.Key))
			return
		}
		if slices.Contains(rule.Hidden, rule.Key) {
			fail(fmt.Sprintf("%s: the key column %s can't be hidden", tab[0], rule.Key))
			return
		}
		tabs[tab[0]] = rule
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
