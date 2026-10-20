package sheetserve

import (
	"cmp"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2"
)

//go:embed web.html
var webHTML string

var webTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"has": slices.Contains[[]string], "join": strings.Join,
}).Parse(webHTML))

var sheetName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type user struct {
	id    int64
	email string
}

type page struct {
	Email, Error, NewKey, PickerKey, AppID string
	Sheets                                 []sheetView
	Keys                                   []keyView
	TabSpecs, AllOps                       []string
}

type sheetView struct {
	Name, Title string
	Tabs        []tabView
}

type tabView struct {
	Name   string
	Ops    []string
	Hidden string
}

type keyView struct{ Name, Tabs, Ops string }

// web serves the dashboard where signed-in users manage their spreadsheets, tabs and keys.
func (m *multiTenant) web() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if id, email, ok := m.currentUser(r); ok {
			m.render(w, r, user{id, email}, page{}, http.StatusOK)
			return
		}
		webTmpl.ExecuteTemplate(w, "login", nil)
	})
	mux.HandleFunc("POST /logout", m.logout)
	mux.HandleFunc("GET /app/token", m.signedIn(m.token))
	mux.HandleFunc("POST /app/sheets", m.signedIn(m.addSheet))
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
			writeJSON(w, http.StatusOK, map[string]string{"token": t.AccessToken})
			return
		}
	}
	slog.Error("picker token", "user", u.id, "err", err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not get a Google token; sign in again"})
}

func (m *multiTenant) addSheet(w http.ResponseWriter, r *http.Request, u user) {
	name, gid, title := r.FormValue("name"), r.FormValue("google_id"), r.FormValue("title")
	if !sheetName.MatchString(name) || gid == "" {
		m.render(w, r, u, page{Error: "name must be letters, digits, - or _"}, http.StatusBadRequest)
		return
	}
	t, err := m.tenant(r.Context(), u.id)
	var tabs []string
	if err == nil {
		tabs, err = t.svc.store.Tabs(r.Context(), gid)
	}
	if err != nil {
		slog.Error("read tabs", "user", u.id, "err", err)
		m.render(w, r, u, page{Error: "could not read that spreadsheet from Google"}, http.StatusBadGateway)
		return
	}
	m.change(w, r, u, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO spreadsheets (user_id, name, google_id, title) VALUES (?, ?, ?, ?)`,
			u.id, name, gid, title); err != nil {
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

func (m *multiTenant) deleteSheet(w http.ResponseWriter, r *http.Request, u user) {
	m.change(w, r, u, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM spreadsheets WHERE user_id = ? AND name = ?`, u.id, r.FormValue("name"))
		return err
	})
}

// ponytail: hidden columns are free text; read the header row to offer checkboxes if typos bite.
func (m *multiTenant) saveTab(w http.ResponseWriter, r *http.Request, u user) {
	r.ParseForm()
	var hidden []string
	for h := range strings.SplitSeq(r.FormValue("hidden"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hidden = append(hidden, h)
		}
	}
	m.change(w, r, u, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tabs SET ops = ?, hidden = ? WHERE user_id = ? AND sheet = ? AND tab = ?`,
			jsonList(r.Form["ops"]), jsonList(hidden), u.id, r.FormValue("sheet"), r.FormValue("tab"))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, badRequest("unknown tab"))
		}
		return nil
	})
}

func (m *multiTenant) createKey(w http.ResponseWriter, r *http.Request, u user) {
	r.ParseForm()
	key := newKey()
	err := m.db.update(r.Context(), u.id, m.global, func(tx *sql.Tx) error {
		if len(r.Form["tabs"]) == 0 {
			return badRequest("pick at least one tab for the key")
		}
		_, err := tx.Exec(`INSERT INTO api_keys (hash, user_id, name, tabs, ops) VALUES (?, ?, ?, ?, ?)`,
			hashKey(key), u.id, strings.TrimSpace(r.FormValue("name")), jsonList(r.Form["tabs"]), jsonList(r.Form["ops"]))
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
	var ae *apiError
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
	p.Email, p.PickerKey, p.AppID, p.AllOps = u.email, m.pickerKey, m.appID, allOps
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
	err := scan(ctx, q, `SELECT name, title FROM spreadsheets WHERE user_id = ? ORDER BY name`, userID, func(rows *sql.Rows) error {
		var s sheetView
		if err := rows.Scan(&s.Name, &s.Title); err != nil {
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
		p.Sheets[i].Tabs = append(p.Sheets[i].Tabs, tabView{tab, rule.Ops, strings.Join(rule.Hidden, ", ")})
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
