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
	d.saveSheet(w, r, u, db.Spreadsheet{Name: name, Title: title, GoogleID: gid, Tabs: tabs})
}

// saveSheet records a spreadsheet and its tabs, then shows it on the spreadsheets page.
func (d *Dashboard) saveSheet(w http.ResponseWriter, r *http.Request, u user, s db.Spreadsheet) {
	d.change(w, r, u, sheetsPage+"#sheet-"+s.Name, d.DB.AddSpreadsheet(r.Context(), u.id, s))
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
	tabs, err := parseSchema(form.Schema)
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
	d.saveSheet(w, r, u, db.Spreadsheet{Name: form.Name, Title: title, GoogleID: gid, Tabs: names})
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

// parseSchema reads one tab per line, as "Tab: column, column". Each tab gets an id column first
// unless it lists one, since the API finds rows by id.
func parseSchema(s string) ([]store.Tab, error) {
	var tabs []store.Tab
	for line := range strings.Lines(s) {
		name, cols, _ := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if name == "" && strings.TrimSpace(cols) == "" {
			continue
		}
		switch {
		case name == "":
			return nil, fmt.Errorf("tab name missing in %q", strings.TrimSpace(line))
		case strings.Contains(name, "/"):
			return nil, fmt.Errorf("tab %q: name must not contain /", name)
		case slices.ContainsFunc(tabs, func(t store.Tab) bool { return strings.EqualFold(t.Name, name) }):
			return nil, fmt.Errorf("tab %q is listed twice", name)
		}
		t := store.Tab{Name: name}
		for c := range strings.SplitSeq(cols, ",") {
			if c = strings.TrimSpace(c); c == "" {
				continue
			}
			if slices.Contains(t.Columns, c) {
				return nil, fmt.Errorf("tab %q: column %q is listed twice", name, c)
			}
			t.Columns = append(t.Columns, c)
		}
		if !slices.Contains(t.Columns, "id") {
			t.Columns = slices.Insert(t.Columns, 0, "id")
		}
		tabs = append(tabs, t)
	}
	if len(tabs) == 0 {
		return nil, errors.New("list at least one tab, such as \"Orders: item, qty\"")
	}
	return tabs, nil
}

func (d *Dashboard) deleteSheet(w http.ResponseWriter, r *http.Request, u user) {
	d.change(w, r, u, sheetsPage, d.DB.DeleteSpreadsheet(r.Context(), u.id, r.FormValue("name")))
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
	d.change(w, r, u, sheetsPage+"#sheet-"+sheet, d.DB.SaveTab(r.Context(), u.id, sheet, tab, rule))
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
