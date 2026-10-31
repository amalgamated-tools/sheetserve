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
	// Next, the user reviews the columns of its tabs, with types suggested from their rows.
	d.saveSheet(w, r, u, "sheet.add", db.Spreadsheet{Name: name, Title: title, GoogleID: gid, Tabs: tabs}, columnsURL(name))
}

// saveSheet records a spreadsheet and its tabs, logs action, then shows the page at to.
func (d *Dashboard) saveSheet(w http.ResponseWriter, r *http.Request, u user, action string, s db.Spreadsheet, to string) {
	err := d.DB.AddSpreadsheet(r.Context(), u.id, s)
	if err == nil {
		audit(r.Context(), u, action, slog.String(otelkeys.Sheet, s.Name), slog.String(otelkeys.SpreadsheetID, s.GoogleID),
			slog.String(otelkeys.Tabs, strings.Join(s.Tabs, ",")))
	}
	d.change(w, r, u, to, err)
}

// createSheet makes a blank spreadsheet in the user's Drive from the tabs and columns on the new
// spreadsheet page, then adds it like a picked one, with its key columns and column types saved.
// drive.file covers files the app creates, so the user needn't pick it.
func (d *Dashboard) createSheet(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		d.render(w, r, u, view{path: newSheetPage, err: "could not read the form"}, http.StatusBadRequest)
		return
	}
	form := newSheetForm{Name: r.PostForm.Get("name"), Title: strings.TrimSpace(r.PostForm.Get("title")), Tabs: parseTabs(r.PostForm)}
	fail := func(status int, msg string) {
		d.render(w, r, u, view{path: newSheetPage, err: msg, newSheet: form}, status)
	}
	if !sheetName.MatchString(form.Name) {
		fail(http.StatusBadRequest, "name must be letters, digits, - or _")
		return
	}
	tabs, rules, err := newTabs(form.Tabs)
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
	d.saveSheet(w, r, u, "sheet.create", db.Spreadsheet{Name: form.Name, Title: title, GoogleID: gid, Tabs: names, Rules: rules},
		sheetsPage+"#sheet-"+form.Name)
}

// newTabs turns the tabs on the new spreadsheet page into the header rows to write and the rules
// to save, by tab name. A tab without a key column gets an id column first unless it lists one,
// since the API finds its rows by id. A tab with no name and no columns is a blank card, skipped.
func newTabs(forms []tabForm) ([]store.Tab, map[string]db.TabRule, error) {
	var tabs []store.Tab
	rules := map[string]db.TabRule{}
	for _, f := range forms {
		switch {
		case f.Name == "" && len(f.Columns) == 0:
			continue
		case f.Name == "":
			return nil, nil, errors.New("give each tab a name")
		case strings.Contains(f.Name, "/"):
			return nil, nil, fmt.Errorf("tab %q: name must not contain /", f.Name)
		case slices.ContainsFunc(tabs, func(t store.Tab) bool { return strings.EqualFold(t.Name, f.Name) }):
			return nil, nil, fmt.Errorf("tab %q is listed twice", f.Name)
		}
		if f.Key == "" && !slices.ContainsFunc(f.Columns, func(c columnForm) bool { return c.Name == "id" }) {
			f.Columns = slices.Insert(f.Columns, 0, columnForm{Name: "id", Type: "string"})
		}
		rule, err := f.rule()
		if err != nil {
			return nil, nil, err
		}
		t := store.Tab{Name: f.Name}
		for _, c := range f.Columns {
			t.Columns = append(t.Columns, c.Name)
		}
		tabs, rules[f.Name] = append(tabs, t), rule
	}
	if len(tabs) == 0 {
		return nil, nil, errors.New("add at least one tab")
	}
	return tabs, rules, nil
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

func (d *Dashboard) deleteSheet(w http.ResponseWriter, r *http.Request, u user) {
	name := r.FormValue("name")
	err := d.DB.DeleteSpreadsheet(r.Context(), u.id, name)
	if err == nil {
		audit(r.Context(), u, "sheet.delete", slog.String(otelkeys.Sheet, name))
	}
	d.change(w, r, u, sheetsPage, err)
}

// saveOps sets the ops each tab of a spreadsheet allows: the form lists each tab as "tab", and
// its ops as "ops:<tab>". Columns are set on the columns page.
func (d *Dashboard) saveOps(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		d.render(w, r, u, view{path: sheetsPage, err: "could not read the form"}, http.StatusBadRequest)
		return
	}
	sheet, ops := r.FormValue("sheet"), map[string][]string{}
	for _, tab := range r.Form["tab"] {
		ops[tab] = r.Form["ops:"+tab]
	}
	err := d.DB.SaveOps(r.Context(), u.id, sheet, ops)
	if err == nil {
		for _, tab := range r.Form["tab"] {
			audit(r.Context(), u, "tab.save", slog.String(otelkeys.Sheet, sheet), slog.String(otelkeys.Tab, tab),
				slog.String(otelkeys.Ops, strings.Join(ops[tab], ",")))
		}
	}
	d.change(w, r, u, sheetsPage+"#sheet-"+sheet, err)
}
