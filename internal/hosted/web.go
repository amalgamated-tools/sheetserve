package hosted

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

//go:embed web.html
var webHTML string

var webTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"has": slices.Contains[[]string], "join": strings.Join,
}).Parse(webHTML))

var sheetName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var sheetURL = regexp.MustCompile(`/spreadsheets/d/([A-Za-z0-9_-]+)`)

// spreadsheetID takes a spreadsheet URL or a bare id and returns the id, or "".
func spreadsheetID(s string) string {
	s = strings.TrimSpace(s)
	if m := sheetURL.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if sheetName.MatchString(s) {
		return s
	}
	return ""
}

type user struct {
	id    int64
	email string
}

// The dashboard's pages. A form posts to /app/... and lands back on the page it's on.
const (
	sheetsPage   = "/sheets"
	newSheetPage = "/sheets/new"
	keysPage     = "/keys"
)

type page struct {
	Path                                   string // sheetsPage, newSheetPage or keysPage
	Email, Error, NewKey, PickerKey, AppID string
	SAEmail                                string       // set when the server has a service account
	NewSheet                               newSheetForm // refilled after a failed create
	Sheets                                 []sheetView
	Keys                                   []keyView
	TabSpecs, AllOps                       []string
}

// Title names the page in the browser tab.
func (p page) Title() string {
	switch p.Path {
	case keysPage:
		return "API keys"
	case newSheetPage:
		return "New spreadsheet"
	}
	return "Spreadsheets"
}

// Section is the nav entry the page belongs under.
func (p page) Section() string {
	if p.Path == keysPage {
		return keysPage
	}
	return sheetsPage
}

type sheetView struct {
	Name, Title    string
	GoogleID       string // for the link to open it in Google Sheets
	ServiceAccount bool
	Tabs           []tabView
}

type tabView struct {
	Name   string
	Ops    []string
	Hidden string
	Key    string // "" when it's the default, id
}

type keyView struct{ Name, Tabs, Ops string }

type newSheetForm struct{ Name, Title, Schema string }

// web serves the dashboard where signed-in users manage their spreadsheets, tabs and keys.
func (m *multiTenant) web() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := m.currentUser(r); ok {
			http.Redirect(w, r, sheetsPage, http.StatusSeeOther)
			return
		}
		if err := webTmpl.ExecuteTemplate(w, "login", nil); err != nil {
			slog.Error("template", "err", err)
		}
	})
	for _, path := range []string{sheetsPage, newSheetPage, keysPage} {
		mux.HandleFunc("GET "+path, m.signedIn(func(w http.ResponseWriter, r *http.Request, u user) {
			m.render(w, r, u, page{Path: path}, http.StatusOK)
		}))
	}
	mux.HandleFunc("POST /logout", m.logout)
	mux.HandleFunc("GET /app/token", m.signedIn(m.token))
	mux.HandleFunc("POST /app/sheets", m.signedIn(m.addSheet))
	mux.HandleFunc("POST /app/sheets/new", m.signedIn(m.createSheet))
	mux.HandleFunc("POST /app/sheets/delete", m.signedIn(m.deleteSheet))
	mux.HandleFunc("POST /app/tabs", m.signedIn(m.saveTab))
	mux.HandleFunc("POST /app/keys", m.signedIn(m.createKey))
	mux.HandleFunc("POST /app/keys/delete", m.signedIn(m.deleteKey))
	return mux
}

func (m *multiTenant) signedIn(h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, email, ok := m.currentUser(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		h(w, r, user{id, email})
	}
}

// token gives the dashboard a short-lived access token for Google Picker.
func (m *multiTenant) token(w http.ResponseWriter, r *http.Request, u user) {
	ts, err := m.tokenSource(u.id)
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

// addSheet adds a spreadsheet picked from the user's Drive, or, with via=service_account, one
// given by URL that the server reaches with its service account.
func (m *multiTenant) addSheet(w http.ResponseWriter, r *http.Request, u user) {
	name, gid, title := r.FormValue("name"), r.FormValue("google_id"), r.FormValue("title")
	viaSA := r.FormValue("via") == "service_account"
	if viaSA {
		if m.saStore == nil {
			m.render(w, r, u, page{Path: sheetsPage, Error: "this server has no service account"}, http.StatusBadRequest)
			return
		}
		gid, title = spreadsheetID(r.FormValue("url")), name
	}
	if !sheetName.MatchString(name) {
		m.render(w, r, u, page{Path: sheetsPage, Error: "name must be letters, digits, - or _"}, http.StatusBadRequest)
		return
	}
	if gid == "" {
		m.render(w, r, u, page{Path: sheetsPage, Error: "paste a Google Sheets URL"}, http.StatusBadRequest)
		return
	}
	var tabs []string
	var err error
	if viaSA {
		tabs, err = m.saStore.Tabs(r.Context(), gid)
	} else {
		var t *tenant
		if t, err = m.tenant(r.Context(), u.id); err == nil {
			tabs, err = t.svc.Tabs(r.Context(), gid)
		}
	}
	if err != nil {
		slog.Error("read tabs", "user", u.id, "service_account", viaSA, "err", err)
		msg := "could not read that spreadsheet from Google" + googleDetail(err)
		if viaSA {
			msg += "; share it with " + m.saEmail + " first"
		}
		m.render(w, r, u, page{Path: sheetsPage, Error: msg}, http.StatusBadGateway)
		return
	}
	m.saveSheet(w, r, u, db.Spreadsheet{Name: name, Title: title, GoogleID: gid, ServiceAccount: viaSA, Tabs: tabs})
}

// googleDetail returns Google's status and message for err, ready to append to a dashboard error.
func googleDetail(err error) string {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return fmt.Sprintf(" (%d: %s)", gerr.Code, gerr.Message)
	}
	return ""
}

// saveSheet records a spreadsheet and its tabs, then shows it on the spreadsheets page.
func (m *multiTenant) saveSheet(w http.ResponseWriter, r *http.Request, u user, s db.Spreadsheet) {
	m.change(w, r, u, sheetsPage+"#sheet-"+s.Name, m.db.AddSpreadsheet(r.Context(), u.id, s))
}

// createSheet makes a blank spreadsheet in the user's Drive from a schema, then adds it like a
// picked one. drive.file covers files the app creates, so the user needn't pick it.
func (m *multiTenant) createSheet(w http.ResponseWriter, r *http.Request, u user) {
	form := newSheetForm{Name: r.FormValue("name"), Title: strings.TrimSpace(r.FormValue("title")), Schema: r.FormValue("schema")}
	fail := func(status int, msg string) {
		m.render(w, r, u, page{Path: newSheetPage, Error: msg, NewSheet: form}, status)
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
	taken, err := m.db.SpreadsheetExists(r.Context(), u.id, form.Name)
	if err != nil {
		fail(http.StatusInternalServerError, userMessage(err))
		return
	}
	if taken {
		fail(http.StatusBadRequest, "that name is already in use")
		return
	}
	title := cmp.Or(form.Title, form.Name)
	gid, err := m.create(r.Context(), u.id, title, tabs)
	if err != nil {
		slog.Error("create sheet", "user", u.id, "err", err)
		fail(http.StatusBadGateway, "could not create the spreadsheet in Google"+googleDetail(err))
		return
	}
	names := make([]string, len(tabs))
	for i, t := range tabs {
		names[i] = t.Name
	}
	m.saveSheet(w, r, u, db.Spreadsheet{Name: form.Name, Title: title, GoogleID: gid, Tabs: names})
}

// create makes a spreadsheet with the user's own credentials, so it lands in their Drive.
func (m *multiTenant) create(ctx context.Context, userID int64, title string, tabs []store.Tab) (string, error) {
	st, err := m.newStore(userID)
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

func (m *multiTenant) deleteSheet(w http.ResponseWriter, r *http.Request, u user) {
	m.change(w, r, u, sheetsPage, m.db.DeleteSpreadsheet(r.Context(), u.id, r.FormValue("name")))
}

// TODO: hidden columns are free text; read the header row to offer checkboxes if typos bite.
func (m *multiTenant) saveTab(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		m.render(w, r, u, page{Path: sheetsPage, Error: "could not read the form"}, http.StatusBadRequest)
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
		if msg := m.checkKey(r.Context(), u.id, sheet, tab, key); msg != "" {
			m.render(w, r, u, page{Path: sheetsPage, Error: msg}, http.StatusBadRequest)
			return
		}
	}
	rule := config.TabRule{Ops: r.Form["ops"], Hidden: hidden, Key: key}
	m.change(w, r, u, sheetsPage+"#sheet-"+sheet, m.db.SaveTab(r.Context(), u.id, sheet, tab, rule))
}

// checkKey reads the tab's header from Google and returns why key can't be its key column, or "".
func (m *multiTenant) checkKey(ctx context.Context, userID int64, sheet, tab, key string) string {
	t, err := m.tenant(ctx, userID)
	var header []string
	if err == nil {
		header, err = t.svc.Header(ctx, sheet, tab)
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

func (m *multiTenant) createKey(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		m.render(w, r, u, page{Path: keysPage, Error: "could not read the form"}, http.StatusBadRequest)
		return
	}
	if len(r.Form["tabs"]) == 0 {
		m.render(w, r, u, page{Path: keysPage, Error: "pick at least one tab for the key"}, http.StatusBadRequest)
		return
	}
	key := config.NewKey()
	err := m.db.CreateKey(r.Context(), u.id, config.Key{
		Hash: config.HashKey(key), Name: strings.TrimSpace(r.FormValue("name")), Tabs: r.Form["tabs"], Ops: r.Form["ops"],
	})
	if err != nil {
		m.render(w, r, u, page{Path: keysPage, Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	m.invalidate(u.id)
	m.render(w, r, u, page{Path: keysPage, NewKey: key}, http.StatusOK)
}

func (m *multiTenant) deleteKey(w http.ResponseWriter, r *http.Request, u user) {
	m.change(w, r, u, keysPage, m.db.DeleteKey(r.Context(), u.id, r.FormValue("name")))
}

// change finishes a dashboard update that returned err: it redirects to "to" (a page, maybe with a
// fragment), or shows the error on that page.
func (m *multiTenant) change(w http.ResponseWriter, r *http.Request, u user, to string, err error) {
	if err != nil {
		path, _, _ := strings.Cut(to, "#")
		m.render(w, r, u, page{Path: path, Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	m.invalidate(u.id)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func userMessage(err error) string {
	var ae *service.APIError
	switch {
	case errors.As(err, &ae):
		return ae.Msg
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		return "that name is already in use"
	}
	slog.Error("dashboard", "err", err)
	return "something went wrong; try again"
}

func (m *multiTenant) render(w http.ResponseWriter, r *http.Request, u user, p page, status int) {
	if err := m.load(r.Context(), u.id, &p); err != nil {
		slog.Error("dashboard", "user", u.id, "err", err)
		http.Error(w, "could not load your settings", http.StatusInternalServerError)
		return
	}
	p.Email, p.PickerKey, p.AppID, p.SAEmail, p.AllOps = u.email, m.pickerKey, m.appID, m.saEmail, config.AllOps
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := webTmpl.ExecuteTemplate(w, strings.TrimPrefix(p.Path, "/"), p); err != nil {
		slog.Error("template", "err", err)
	}
}

// load fills p with userID's spreadsheets, tabs and keys, in display order.
func (m *multiTenant) load(ctx context.Context, userID int64, p *page) error {
	sheets, cfg, err := m.db.Spreadsheets(ctx, userID)
	if err != nil {
		return err
	}
	p.TabSpecs = []string{"*"}
	for _, s := range sheets {
		p.Sheets = append(p.Sheets, sheetView{Name: s.Name, Title: s.Title, GoogleID: s.GoogleID, ServiceAccount: s.ServiceAccount})
		p.TabSpecs = append(p.TabSpecs, s.Name+"/*")
	}
	for i, s := range sheets {
		for _, tab := range s.Tabs {
			rule := cfg.Spreadsheets[s.Name].Tabs[tab]
			key := rule.KeyColumn()
			if key == "id" {
				key = ""
			}
			p.Sheets[i].Tabs = append(p.Sheets[i].Tabs, tabView{tab, rule.Ops, strings.Join(rule.Hidden, ", "), key})
			p.TabSpecs = append(p.TabSpecs, s.Name+"/"+tab)
		}
	}
	for _, k := range cfg.Keys {
		ops := "all the tab allows"
		if len(k.Ops) > 0 {
			ops = strings.Join(k.Ops, ", ")
		}
		p.Keys = append(p.Keys, keyView{k.Name, strings.Join(k.Tabs, ", "), ops})
	}
	return nil
}
