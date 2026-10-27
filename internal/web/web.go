package web

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
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
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
	role  rbac.Role
}

// The dashboard's pages. A form posts to /app/... and lands back on the page it's on.
const (
	sheetsPage   = "/sheets"
	newSheetPage = "/sheets/new"
	keysPage     = "/keys"
	usersPage    = "/users"
	pendingPage  = "/pending" // shown instead of any page to a user who is not approved yet
)

type page struct {
	Path                                   string // one of the ...Page constants
	Email, Error, NewKey, PickerKey, AppID string
	NewSheet                               newSheetForm // refilled after a failed create
	Sheets                                 []sheetView
	Keys                                   []keyView
	TabSpecs, AllOps                       []string
	Admin                                  bool       // the viewer may manage users
	Users                                  []userView // on usersPage
	Roles                                  []rbac.Role
}

// Title names the page in the browser tab.
func (p page) Title() string {
	switch p.Path {
	case keysPage:
		return "API keys"
	case newSheetPage:
		return "New spreadsheet"
	case usersPage:
		return "Users"
	case pendingPage:
		return "Waiting for approval"
	}
	return "Spreadsheets"
}

// Section is the nav entry the page belongs under.
func (p page) Section() string {
	if p.Path == keysPage || p.Path == usersPage {
		return p.Path
	}
	return sheetsPage
}

type sheetView struct {
	Name, Title string
	GoogleID    string // for the link to open it in Google Sheets
	Tabs        []tabView
}

type tabView struct {
	Name   string
	Ops    []string
	Hidden string
	Key    string // "" when it's the default, id
}

type keyView struct{ Name, Tabs, Ops string }

type userView struct {
	ID    int64
	Email string
	Role  rbac.Role
	You   bool
}

type newSheetForm struct{ Name, Title, Schema string }

// Dashboard serves the pages where signed-in users manage their spreadsheets, tabs and keys.
type Dashboard struct {
	DB        *db.DB
	Auth      *auth.Google
	Tenants   *tenancy.Tenants
	PickerKey string // browser API key for Google Picker
	AppID     string // Cloud project number, so picked files are granted to this app
}

// Routes adds the dashboard's pages and forms to r. It refuses cross-origin form posts.
func (d *Dashboard) Routes(r chi.Router) {
	r.Group(func(mux chi.Router) {
		mux.Use(http.NewCrossOriginProtection().Handler)
		d.routes(mux)
	})
}

func (d *Dashboard) routes(mux chi.Router) {
	mux.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.Auth.CurrentUser(r); ok {
			http.Redirect(w, r, sheetsPage, http.StatusSeeOther)
			return
		}
		if err := webTmpl.ExecuteTemplate(w, "login", nil); err != nil {
			slog.Error("template", "err", err)
		}
	})
	for _, path := range []string{sheetsPage, newSheetPage, keysPage} {
		mux.Get(path, d.signedIn(rbac.EditOwn, func(w http.ResponseWriter, r *http.Request, u user) {
			d.render(w, r, u, page{Path: path}, http.StatusOK)
		}))
	}
	mux.Get(usersPage, d.signedIn(rbac.ManageUsers, func(w http.ResponseWriter, r *http.Request, u user) {
		d.render(w, r, u, page{Path: usersPage}, http.StatusOK)
	}))
	mux.Get("/app/token", d.signedIn(rbac.EditOwn, d.token))
	mux.Post("/app/sheets", d.signedIn(rbac.EditOwn, d.addSheet))
	mux.Post("/app/sheets/new", d.signedIn(rbac.EditOwn, d.createSheet))
	mux.Post("/app/sheets/delete", d.signedIn(rbac.EditOwn, d.deleteSheet))
	mux.Post("/app/tabs", d.signedIn(rbac.EditOwn, d.saveTab))
	mux.Post("/app/keys", d.signedIn(rbac.EditOwn, d.createKey))
	mux.Post("/app/keys/delete", d.signedIn(rbac.EditOwn, d.deleteKey))
	mux.Post("/app/users/role", d.signedIn(rbac.ManageUsers, d.setRole))
	mux.Post("/app/users/delete", d.signedIn(rbac.ManageUsers, d.deleteUser))
}

// signedIn runs h for a signed-in user whose role grants perm. A user still waiting for approval
// sees the pending page instead.
func (d *Dashboard) signedIn(perm rbac.Permission, h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cur, ok := d.Auth.CurrentUser(r)
		if !ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		u := user{cur.ID, cur.Email, cur.Role}
		switch {
		case u.role.Can(perm):
			h(w, r, u)
		case !u.role.Can(rbac.EditOwn):
			d.render(w, r, u, page{Path: pendingPage}, http.StatusForbidden)
		default:
			http.Error(w, "your role does not allow that", http.StatusForbidden)
		}
	}
}

// setRole changes another user's role, such as approving a pending one.
func (d *Dashboard) setRole(w http.ResponseWriter, r *http.Request, u user) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err == nil {
		err = d.DB.SetRole(r.Context(), id, rbac.Role(r.FormValue("role")))
	}
	d.change(w, r, u, usersPage, err)
}

// deleteUser removes a user and everything they added. Their keys stop working at once.
func (d *Dashboard) deleteUser(w http.ResponseWriter, r *http.Request, u user) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err == nil {
		err = d.DB.DeleteUser(r.Context(), id)
	}
	if err == nil {
		d.Tenants.Invalidate(id)
	}
	d.change(w, r, u, usersPage, err)
}

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
		d.render(w, r, u, page{Path: sheetsPage, Error: "name must be letters, digits, - or _"}, http.StatusBadRequest)
		return
	}
	if gid == "" {
		d.render(w, r, u, page{Path: sheetsPage, Error: "choose a spreadsheet from Google Drive"}, http.StatusBadRequest)
		return
	}
	var tabs []string
	svc, err := d.Tenants.Service(r.Context(), u.id)
	if err == nil {
		tabs, err = svc.Tabs(r.Context(), gid)
	}
	if err != nil {
		slog.Error("read tabs", "user", u.id, "err", err)
		d.render(w, r, u, page{Path: sheetsPage, Error: "could not read that spreadsheet from Google" + googleDetail(err)}, http.StatusBadGateway)
		return
	}
	d.saveSheet(w, r, u, db.Spreadsheet{Name: name, Title: title, GoogleID: gid, Tabs: tabs})
}

// googleDetail returns Google's status and message for err, ready to append to a dashboard error.
func googleDetail(err error) string {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return fmt.Sprintf(" (%d: %s)", gerr.Code, gerr.Message)
	}
	return ""
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
		d.render(w, r, u, page{Path: newSheetPage, Error: msg, NewSheet: form}, status)
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
		d.render(w, r, u, page{Path: sheetsPage, Error: "could not read the form"}, http.StatusBadRequest)
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
			d.render(w, r, u, page{Path: sheetsPage, Error: msg}, http.StatusBadRequest)
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

func (d *Dashboard) createKey(w http.ResponseWriter, r *http.Request, u user) {
	if err := r.ParseForm(); err != nil {
		d.render(w, r, u, page{Path: keysPage, Error: "could not read the form"}, http.StatusBadRequest)
		return
	}
	if len(r.Form["tabs"]) == 0 {
		d.render(w, r, u, page{Path: keysPage, Error: "pick at least one tab for the key"}, http.StatusBadRequest)
		return
	}
	key := db.NewKey()
	err := d.DB.CreateKey(r.Context(), u.id, db.Key{
		Hash: db.HashKey(key), Name: strings.TrimSpace(r.FormValue("name")), Tabs: r.Form["tabs"], Ops: r.Form["ops"],
	})
	if err != nil {
		d.render(w, r, u, page{Path: keysPage, Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	d.Tenants.Invalidate(u.id)
	d.render(w, r, u, page{Path: keysPage, NewKey: key}, http.StatusOK)
}

func (d *Dashboard) deleteKey(w http.ResponseWriter, r *http.Request, u user) {
	d.change(w, r, u, keysPage, d.DB.DeleteKey(r.Context(), u.id, r.FormValue("name")))
}

// change finishes a dashboard update that returned err: it redirects to "to" (a page, maybe with a
// fragment), or shows the error on that page.
func (d *Dashboard) change(w http.ResponseWriter, r *http.Request, u user, to string, err error) {
	if err != nil {
		path, _, _ := strings.Cut(to, "#")
		d.render(w, r, u, page{Path: path, Error: userMessage(err)}, http.StatusBadRequest)
		return
	}
	d.Tenants.Invalidate(u.id)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func userMessage(err error) string {
	var ae *service.APIError
	var ve db.ValidationError
	switch {
	case errors.As(err, &ae):
		return ae.Msg
	case errors.As(err, &ve):
		return ve.Error()
	case strings.Contains(err.Error(), "UNIQUE constraint failed"):
		return "that name is already in use"
	}
	slog.Error("dashboard", "err", err)
	return "something went wrong; try again"
}

func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, u user, p page, status int) {
	if err := d.load(r.Context(), u.id, &p); err != nil {
		slog.Error("dashboard", "user", u.id, "err", err)
		http.Error(w, "could not load your settings", http.StatusInternalServerError)
		return
	}
	p.Email, p.PickerKey, p.AppID, p.AllOps = u.email, d.PickerKey, d.AppID, db.AllOps
	p.Admin, p.Roles = u.role.Can(rbac.ManageUsers), rbac.Roles
	if p.Path == usersPage {
		users, err := d.DB.Users(r.Context())
		if err != nil {
			slog.Error("dashboard", "user", u.id, "err", err)
			http.Error(w, "could not load the users", http.StatusInternalServerError)
			return
		}
		for _, v := range users {
			p.Users = append(p.Users, userView{v.ID, v.Email, v.Role, v.ID == u.id})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := webTmpl.ExecuteTemplate(w, strings.TrimPrefix(p.Path, "/"), p); err != nil {
		slog.Error("template", "err", err)
	}
}

// load fills p with userID's spreadsheets, tabs and keys, in display order.
func (d *Dashboard) load(ctx context.Context, userID int64, p *page) error {
	sheets, settings, err := d.DB.Spreadsheets(ctx, userID)
	if err != nil {
		return err
	}
	p.TabSpecs = []string{"*"}
	for _, s := range sheets {
		p.Sheets = append(p.Sheets, sheetView{Name: s.Name, Title: s.Title, GoogleID: s.GoogleID})
		p.TabSpecs = append(p.TabSpecs, s.Name+"/*")
	}
	for i, s := range sheets {
		for _, tab := range s.Tabs {
			rule := settings.Spreadsheets[s.Name].Tabs[tab]
			key := rule.KeyColumn()
			if key == "id" {
				key = ""
			}
			p.Sheets[i].Tabs = append(p.Sheets[i].Tabs, tabView{tab, rule.Ops, strings.Join(rule.Hidden, ", "), key})
			p.TabSpecs = append(p.TabSpecs, s.Name+"/"+tab)
		}
	}
	for _, k := range settings.Keys {
		ops := "all the tab allows"
		if len(k.Ops) > 0 {
			ops = strings.Join(k.Ops, ", ")
		}
		p.Keys = append(p.Keys, keyView{k.Name, strings.Join(k.Tabs, ", "), ops})
	}
	return nil
}
