package hosted

import (
	"cmp"
	"context"
	"database/sql"
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

type page struct {
	Email, Error, NewKey, PickerKey, AppID string
	SAEmail                                string       // set when the server has a service account
	NewSheet                               newSheetForm // refilled after a failed create
	Sheets                                 []sheetView
	Keys                                   []keyView
	TabSpecs, AllOps                       []string
}

type sheetView struct {
	Name, Title    string
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
		if id, email, ok := m.currentUser(r); ok {
			m.render(w, r, user{id, email}, page{}, http.StatusOK)
			return
		}
		if err := webTmpl.ExecuteTemplate(w, "login", nil); err != nil {
			slog.Error("template", "err", err)
		}
	})
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
			m.render(w, r, u, page{Error: "this server has no service account"}, http.StatusBadRequest)
			return
		}
		gid, title = spreadsheetID(r.FormValue("url")), name
	}
	if !sheetName.MatchString(name) {
		m.render(w, r, u, page{Error: "name must be letters, digits, - or _"}, http.StatusBadRequest)
		return
	}
	if gid == "" {
		m.render(w, r, u, page{Error: "paste a Google Sheets URL"}, http.StatusBadRequest)
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
		m.render(w, r, u, page{Error: msg}, http.StatusBadGateway)
		return
	}
	m.saveSheet(w, r, u, name, gid, title, viaSA, tabs)
}

// googleDetail returns Google's status and message for err, ready to append to a dashboard error.
func googleDetail(err error) string {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return fmt.Sprintf(" (%d: %s)", gerr.Code, gerr.Message)
	}
	return ""
}

// saveSheet records a spreadsheet and its tabs, then redirects to the dashboard.
func (m *multiTenant) saveSheet(w http.ResponseWriter, r *http.Request, u user, name, gid, title string, viaSA bool, tabs []string) {
	m.change(w, r, u, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO spreadsheets (user_id, name, google_id, title, service_account) VALUES (?, ?, ?, ?, ?)`,
			u.id, name, gid, title, viaSA); err != nil {
			return err
		}
		for _, tab := range tabs { // every tab starts off; the user enables ops per tab
			if _, err := tx.Exec(`INSERT INTO tabs (user_id, sheet, tab, ops, hidden) VALUES (?, ?, ?, '[]', '[]')`,
				u.id, name, tab); err != nil {
				return err
			}
		}
		return nil
	})
}

// createSheet makes a blank spreadsheet in the user's Drive from a schema, then adds it like a
// picked one. drive.file covers files the app creates, so the user needn't pick it.
func (m *multiTenant) createSheet(w http.ResponseWriter, r *http.Request, u user) {
	form := newSheetForm{Name: r.FormValue("name"), Title: strings.TrimSpace(r.FormValue("title")), Schema: r.FormValue("schema")}
	fail := func(status int, msg string) { m.render(w, r, u, page{Error: msg, NewSheet: form}, status) }
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
	var taken bool
	if err := m.db.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM spreadsheets WHERE user_id = ? AND name = ?)`,
		u.id, form.Name).Scan(&taken); err != nil {
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
	m.saveSheet(w, r, u, form.Name, gid, title, false, names)
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
	m.change(w, r, u, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM spreadsheets WHERE user_id = ? AND name = ?`, u.id, r.FormValue("name"))
		return err
	})
}

// TODO: hidden columns are free text; read the header row to offer checkboxes if typos bite.
func (m *multiTenant) saveTab(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		m.render(w, r, u, page{Error: "could not read the form"}, http.StatusBadRequest)
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
			m.render(w, r, u, page{Error: msg}, http.StatusBadRequest)
			return
		}
	}
	m.change(w, r, u, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tabs SET ops = ?, hidden = ?, key_column = ? WHERE user_id = ? AND sheet = ? AND tab = ?`,
			jsonList(r.Form["ops"]), jsonList(hidden), key, u.id, sheet, tab)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, service.BadRequest("unknown tab"))
		}
		return nil
	})
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
		m.render(w, r, u, page{Error: "could not read the form"}, http.StatusBadRequest)
		return
	}
	key := config.NewKey()
	err := m.db.update(r.Context(), u.id, m.global, func(tx *sql.Tx) error {
		if len(r.Form["tabs"]) == 0 {
			return service.BadRequest("pick at least one tab for the key")
		}
		_, err := tx.Exec(`INSERT INTO api_keys (hash, user_id, name, tabs, ops) VALUES (?, ?, ?, ?, ?)`,
			config.HashKey(key), u.id, strings.TrimSpace(r.FormValue("name")), jsonList(r.Form["tabs"]), jsonList(r.Form["ops"]))
		return err
	})
	if err != nil {
		m.render(w, r, u, page{Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	m.invalidate(u.id)
	m.render(w, r, u, page{NewKey: key}, http.StatusOK)
}

func (m *multiTenant) deleteKey(w http.ResponseWriter, r *http.Request, u user) {
	m.change(w, r, u, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM api_keys WHERE user_id = ? AND name = ?`, u.id, r.FormValue("name"))
		return err
	})
}

// change applies fn as a validated update, then redirects to the dashboard or shows the error.
func (m *multiTenant) change(w http.ResponseWriter, r *http.Request, u user, fn func(tx *sql.Tx) error) {
	if err := m.db.update(r.Context(), u.id, m.global, fn); err != nil {
		m.render(w, r, u, page{Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	m.invalidate(u.id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
	if err := m.db.snapshot(r.Context(), func(tx *sql.Tx) error { return m.load(r.Context(), tx, u.id, &p) }); err != nil {
		slog.Error("dashboard", "user", u.id, "err", err)
		http.Error(w, "could not load your settings", http.StatusInternalServerError)
		return
	}
	p.Email, p.PickerKey, p.AppID, p.SAEmail, p.AllOps = u.email, m.pickerKey, m.appID, m.saEmail, config.AllOps
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := webTmpl.ExecuteTemplate(w, "app", p); err != nil {
		slog.Error("template", "err", err)
	}
}

// load fills p with userID's spreadsheets, tabs and keys, in display order. q must be one snapshot,
// or a sheet added between the reads would leave its tabs without a sheet.
func (m *multiTenant) load(ctx context.Context, q querier, userID int64, p *page) error {
	p.TabSpecs = []string{"*"}
	err := scan(ctx, q, `SELECT name, title, service_account FROM spreadsheets WHERE user_id = ? ORDER BY name`, userID, func(rows *sql.Rows) error {
		var s sheetView
		if err := rows.Scan(&s.Name, &s.Title, &s.ServiceAccount); err != nil {
			return err
		}
		p.Sheets = append(p.Sheets, s)
		p.TabSpecs = append(p.TabSpecs, s.Name+"/*")
		return nil
	})
	if err != nil {
		return err
	}
	cfg, err := userConfig(ctx, q, userID, m.global)
	if err != nil {
		return err
	}
	// rowid keeps tabs in the order Google listed them.
	err = scan(ctx, q, `SELECT sheet, tab FROM tabs WHERE user_id = ? ORDER BY rowid`, userID, func(rows *sql.Rows) error {
		var sheet, tab string
		if err := rows.Scan(&sheet, &tab); err != nil {
			return err
		}
		rule := cfg.Spreadsheets[sheet].Tabs[tab]
		i := slices.IndexFunc(p.Sheets, func(s sheetView) bool { return s.Name == sheet })
		key := rule.KeyColumn()
		if key == "id" {
			key = ""
		}
		p.Sheets[i].Tabs = append(p.Sheets[i].Tabs, tabView{tab, rule.Ops, strings.Join(rule.Hidden, ", "), key})
		p.TabSpecs = append(p.TabSpecs, sheet+"/"+tab)
		return nil
	})
	for _, k := range cfg.Keys {
		ops := "all the tab allows"
		if len(k.Ops) > 0 {
			ops = strings.Join(k.Ops, ", ")
		}
		p.Keys = append(p.Keys, keyView{k.Name, strings.Join(k.Tabs, ", "), ops})
	}
	return err
}
