package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

var sheetName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// token gives the dashboard a short-lived access token for Google Picker.
func (d *Dashboard) token(w http.ResponseWriter, r *http.Request, u user) {
	ts, err := d.Auth.TokenSource(u.id)
	if err == nil {
		var t *oauth2.Token
		if t, err = ts.Token(); err == nil {
			api.WriteJSON(w, http.StatusOK, map[string]string{"token": t.AccessToken})
			return
		}
	}
	slog.Error("picker token", "user", u.id, "err", err)
	api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "could not get a Google token; sign in again"})
}

// addSheet adds a spreadsheet picked from the user's Drive.
func (d *Dashboard) addSheet(w http.ResponseWriter, r *http.Request, u user) {
	name, gid, title := r.FormValue("name"), r.FormValue("google_id"), r.FormValue("title")
	if !sheetName.MatchString(name) {
		d.render(w, r, u, view{path: sheetsPage, err: "name must be letters, digits, - or _"}, http.StatusBadRequest)
		return
	}
	if gid == "" {
		d.render(w, r, u, view{path: sheetsPage, err: "choose a spreadsheet from Google Drive"}, http.StatusBadRequest)
		return
	}
	var tabs []string
	svc, err := d.Tenants.Service(r.Context(), u.id)
	if err == nil {
		tabs, err = svc.Tabs(r.Context(), gid)
	}
	if err != nil {
		slog.Error("read tabs", "user", u.id, "err", err)
		d.render(w, r, u, view{path: sheetsPage, err: "could not read that spreadsheet from Google" + googleDetail(err)}, http.StatusBadGateway)
		return
	}
	d.saveSheet(w, r, u, "sheet.add", db.Spreadsheet{Name: name, Title: title, GoogleID: gid, Tabs: tabs})
}

// saveSheet records a spreadsheet and its tabs, logs action, then shows it on the spreadsheets page.
func (d *Dashboard) saveSheet(w http.ResponseWriter, r *http.Request, u user, action string, s db.Spreadsheet) {
	err := d.DB.AddSpreadsheet(r.Context(), u.id, s)
	if err == nil {
		audit(r.Context(), u, action, slog.String(otelkeys.Sheet, s.Name), slog.String(otelkeys.SpreadsheetID, s.GoogleID),
			slog.String(otelkeys.Tabs, strings.Join(s.Tabs, ",")))
	}
	d.change(w, r, u, sheetsPage+"#sheet-"+s.Name, err)
}

// createSheet makes a blank spreadsheet in the user's Drive from a schema, then adds it like a
// picked one. drive.file covers files the app creates, so the user needn't pick it.
func (d *Dashboard) createSheet(w http.ResponseWriter, r *http.Request, u user) {
	form := newSheetForm{Name: r.FormValue("name"), Title: strings.TrimSpace(r.FormValue("title")), Schema: r.FormValue("schema")}
	fail := func(status int, msg string) {
		d.render(w, r, u, view{path: newSheetPage, err: msg, newSheet: form}, status)
	}
	if !sheetName.MatchString(form.Name) {
		fail(http.StatusBadRequest, "name must be letters, digits, - or _")
		return
	}
	tabs, keys, err := parseSchema(form.Schema)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	// Check the name before creating, so a taken name doesn't leave an unused spreadsheet in Drive.
	taken, err := d.DB.SpreadsheetExists(r.Context(), u.id, form.Name)
	if err != nil {
		fail(http.StatusInternalServerError, userMessage(err))
		return
	}
	if taken {
		fail(http.StatusBadRequest, "that name is already in use")
		return
	}
	title := cmp.Or(form.Title, form.Name)
	gid, err := d.create(r.Context(), u.id, title, tabs)
	if err != nil {
		slog.Error("create sheet", "user", u.id, "err", err)
		fail(http.StatusBadGateway, "could not create the spreadsheet in Google"+googleDetail(err))
		return
	}
	names := make([]string, len(tabs))
	for i, t := range tabs {
		names[i] = t.Name
	}
	d.saveSheet(w, r, u, "sheet.create", db.Spreadsheet{Name: form.Name, Title: title, GoogleID: gid, Tabs: names, Keys: keys})
}

// create makes a spreadsheet with the user's own credentials, so it lands in their Drive.
func (d *Dashboard) create(ctx context.Context, userID int64, title string, tabs []store.Tab) (string, error) {
	st, err := d.Tenants.NewStore(userID)
	if err != nil {
		return "", err
	}
	c, ok := st.(store.Creator)
	if !ok {
		return "", errors.New("store cannot create spreadsheets")
	}
	return c.Create(ctx, title, tabs)
}

// parseSchema reads one tab per line, as "Tab: column, column". A column marked "*column" is the
// tab's key column, returned in keys by tab name. A tab without one gets an id column first unless
// it lists one, since the API finds its rows by id.
func parseSchema(s string) (tabs []store.Tab, keys map[string]string, err error) {
	for line := range strings.Lines(s) {
		name, cols, _ := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if name == "" && strings.TrimSpace(cols) == "" {
			continue
		}
		switch {
		case name == "":
			return nil, nil, fmt.Errorf("tab name missing in %q", strings.TrimSpace(line))
		case strings.Contains(name, "/"):
			return nil, nil, fmt.Errorf("tab %q: name must not contain /", name)
		case slices.ContainsFunc(tabs, func(t store.Tab) bool { return strings.EqualFold(t.Name, name) }):
			return nil, nil, fmt.Errorf("tab %q is listed twice", name)
		}
		t, key := store.Tab{Name: name}, ""
		for c := range strings.SplitSeq(cols, ",") {
			if c = strings.TrimSpace(c); c == "" {
				continue
			}
			if k, ok := strings.CutPrefix(c, "*"); ok {
				if c = strings.TrimSpace(k); c == "" {
					return nil, nil, fmt.Errorf("tab %q: * needs a column name after it", name)
				}
				if key != "" {
					return nil, nil, fmt.Errorf("tab %q: only one column can be marked * as the key", name)
				}
				key = c
			}
			if slices.Contains(t.Columns, c) {
				return nil, nil, fmt.Errorf("tab %q: column %q is listed twice", name, c)
			}
			t.Columns = append(t.Columns, c)
		}
		if key != "" {
			if keys == nil {
				keys = map[string]string{}
			}
			keys[name] = key
		} else if !slices.Contains(t.Columns, "id") {
			t.Columns = slices.Insert(t.Columns, 0, "id")
		}
		tabs = append(tabs, t)
	}
	if len(tabs) == 0 {
		return nil, nil, errors.New("list at least one tab, such as \"Orders: item, qty\"")
	}
	return tabs, keys, nil
}

// opsPreset is a named set of ops that a tab row's menu turns on at once. Ops lists them in
// db.AllOps order, joined by commas.
type opsPreset struct{ Label, Ops string }

var opsPresets = []opsPreset{
	{"None", ""},
	{"Read only", "read"},
	{"Read and write", "read,create,update"},
	{"All", strings.Join(db.AllOps, ",")},
}

// presetValue returns the menu value that matches ops, or "custom" when no preset does.
func presetValue(ops []string) string {
	var in []string
	for _, op := range db.AllOps {
		if slices.Contains(ops, op) {
			in = append(in, op)
		}
	}
	v := strings.Join(in, ",")
	if slices.ContainsFunc(opsPresets, func(p opsPreset) bool { return p.Ops == v }) {
		return v
	}
	return "custom"
}

func (d *Dashboard) deleteSheet(w http.ResponseWriter, r *http.Request, u user) {
	name := r.FormValue("name")
	err := d.DB.DeleteSpreadsheet(r.Context(), u.id, name)
	if err == nil {
		audit(r.Context(), u, "sheet.delete", slog.String(otelkeys.Sheet, name))
	}
	d.change(w, r, u, sheetsPage, err)
}

// TODO: hidden columns are free text; read the header row to offer checkboxes if typos bite.
func (d *Dashboard) saveTab(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		d.render(w, r, u, view{path: sheetsPage, err: "could not read the form"}, http.StatusBadRequest)
		return
	}
	var hidden []string
	for h := range strings.SplitSeq(r.FormValue("hidden"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hidden = append(hidden, h)
		}
	}
	sheet, tab := r.FormValue("sheet"), r.FormValue("tab")
	key := cmp.Or(strings.TrimSpace(r.FormValue("key")), "id")
	// A tab without an id column is fine (it can only be listed), but a key someone typed should exist.
	if key != "id" {
		if msg := d.checkKey(r.Context(), u.id, sheet, tab, key); msg != "" {
			d.render(w, r, u, view{path: sheetsPage, err: msg}, http.StatusBadRequest)
			return
		}
	}
	rule := db.TabRule{Ops: r.Form["ops"], Hidden: hidden, Key: key}
	err := d.DB.SaveTab(r.Context(), u.id, sheet, tab, rule)
	if err == nil {
		audit(r.Context(), u, "tab.save", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab),
			slog.String(otelkeys.Ops, strings.Join(rule.Ops, ",")), slog.String(otelkeys.Hidden, strings.Join(hidden, ",")),
			slog.String(otelkeys.KeyColumn, key))
	}
	d.change(w, r, u, sheetsPage+"#sheet-"+sheet, err)
}

// checkKey reads the tab's header from Google and returns why key can't be its key column, or "".
func (d *Dashboard) checkKey(ctx context.Context, userID int64, sheet, tab, key string) string {
	svc, err := d.Tenants.Service(ctx, userID)
	var header []string
	if err == nil {
		header, err = svc.Header(ctx, sheet, tab)
	}
	switch {
	case err != nil:
		slog.Error("read header", "user", userID, "sheet", sheet, "tab", tab, "err", err)
		return "could not read " + sheet + "/" + tab + " from Google to check its key column" + googleDetail(err)
	case !slices.Contains(header, key):
		return fmt.Sprintf("%s/%s has no %q column in row 1", sheet, tab, key)
	}
	return ""
}
