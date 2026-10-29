package web

//go:generate go tool templ generate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/otelkeys"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
)

//go:embed static
var static embed.FS

type user struct {
	id    int64
	email string
	role  rbac.Role
}

// The dashboard's pages. A form posts to /app/... and lands back on the page it's on.
const (
	sheetsPage   = "/sheets"
	newSheetPage = "/sheets/new"
	columnsPage  = "/sheets/columns" // ?sheet=<name>
	keysPage     = "/keys"
	usersPage    = "/users"
	pendingPage  = "/pending" // shown instead of any page to a user who is not approved yet
)

// view is what a handler shows: one of the ...Page constants, plus messages and form values for it.
type view struct {
	path     string
	err      string
	newKey   string       // on keysPage, just after it's created
	newSheet newSheetForm // on newSheetPage, refilled after a failed create
}

// shell is what layout needs on every signed-in page.
type shell struct {
	Title, Section, Email string
	Admin                 bool // the viewer may manage users
	SheetCount, KeyCount  int
	Error, NewKey         string
}

type sheetView struct {
	Name, Title string
	GoogleID    string // for the link to open it in Google Sheets
	Tabs        []tabView
}

type tabView struct {
	Name   string
	Ops    []string
	Key    string // the key column it names; whether the header has it is shown on the columns page
	Typed  int    // how many of its columns have a type
	Hidden int    // how many of its columns are hidden
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
	mux.Method(http.MethodGet, "/static/*", http.FileServerFS(static))
	mux.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.Auth.CurrentUser(r); ok {
			http.Redirect(w, r, sheetsPage, http.StatusSeeOther)
			return
		}
		write(w, r, loginView(), http.StatusOK)
	})
	for _, path := range []string{sheetsPage, newSheetPage, keysPage} {
		mux.Get(path, d.signedIn(rbac.EditOwn, func(w http.ResponseWriter, r *http.Request, u user) {
			d.render(w, r, u, view{path: path}, http.StatusOK)
		}))
	}
	mux.Get(columnsPage, d.signedIn(rbac.EditOwn, d.columns))
	mux.Get(usersPage, d.signedIn(rbac.ManageUsers, func(w http.ResponseWriter, r *http.Request, u user) {
		d.render(w, r, u, view{path: usersPage}, http.StatusOK)
	}))
	mux.Get("/app/token", d.signedIn(rbac.EditOwn, d.token))
	mux.Post("/app/sheets", d.signedIn(rbac.EditOwn, d.addSheet))
	mux.Post("/app/sheets/new", d.signedIn(rbac.EditOwn, d.createSheet))
	mux.Post("/app/sheets/delete", d.signedIn(rbac.EditOwn, d.deleteSheet))
	mux.Post("/app/tabs", d.signedIn(rbac.EditOwn, d.saveTab))
	mux.Post("/app/sheets/columns", d.signedIn(rbac.EditOwn, d.saveColumns))
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
			d.render(w, r, u, view{path: pendingPage}, http.StatusForbidden)
		default:
			http.Error(w, "your role does not allow that", http.StatusForbidden)
		}
	}
}

// change finishes a dashboard update that returned err: it redirects to "to" (a page, maybe with a
// fragment), or shows the error on that page.
func (d *Dashboard) change(w http.ResponseWriter, r *http.Request, u user, to string, err error) {
	if err != nil {
		path, _, _ := strings.Cut(to, "#")
		d.render(w, r, u, view{path: path, err: userMessage(err)}, http.StatusBadRequest)
		return
	}
	d.Tenants.Invalidate(u.id)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// audit logs a change u made in the dashboard. Call it only once the change has been saved.
func audit(ctx context.Context, u user, action string, attrs ...slog.Attr) {
	slog.LogAttrs(ctx, slog.LevelInfo, "audit", append([]slog.Attr{
		slog.String(otelkeys.Action, action),
		slog.Int64(otelkeys.UserID, u.id),
		slog.String(otelkeys.UserEmail, u.email),
	}, attrs...)...)
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

// googleDetail returns Google's status and message for err, ready to append to a dashboard error.
func googleDetail(err error) string {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return fmt.Sprintf(" (%d: %s)", gerr.Code, gerr.Message)
	}
	return ""
}

// render shows v to u: it loads u's spreadsheets, tabs and keys, and fills in v's page.
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, u user, v view, status int) {
	if v.path == pendingPage {
		write(w, r, pendingView(u.email), status)
		return
	}
	sheets, keys, tabSpecs, err := d.load(r.Context(), u.id)
	if err != nil {
		slog.Error("dashboard", "user", u.id, "err", err)
		http.Error(w, "could not load your settings", http.StatusInternalServerError)
		return
	}
	s := shell{
		Section: sheetsPage, Email: u.email, Admin: u.role.Can(rbac.ManageUsers),
		SheetCount: len(sheets), KeyCount: len(keys), Error: v.err, NewKey: v.newKey,
	}
	var page templ.Component
	switch v.path {
	case newSheetPage:
		s.Title = "New spreadsheet"
		page = newSheetView(s, v.newSheet)
	case keysPage:
		s.Title, s.Section = "API keys", keysPage
		page = keysView(s, keys, tabSpecs)
	case usersPage:
		s.Title, s.Section = "Users", usersPage
		users, err := d.users(r.Context(), u.id)
		if err != nil {
			slog.Error("dashboard", "user", u.id, "err", err)
			http.Error(w, "could not load the users", http.StatusInternalServerError)
			return
		}
		page = usersView(s, users)
	default:
		s.Title = "Spreadsheets"
		page = sheetsView(s, sheets, d.PickerKey, d.AppID)
	}
	write(w, r, page, status)
}

func write(w http.ResponseWriter, r *http.Request, page templ.Component, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.Render(r.Context(), w); err != nil {
		slog.Error("template", "err", err)
	}
}

// load returns userID's spreadsheets and keys in display order, and the tab scopes a key can have.
func (d *Dashboard) load(ctx context.Context, userID int64) ([]sheetView, []keyView, []string, error) {
	sheets, settings, err := d.DB.Spreadsheets(ctx, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	views := make([]sheetView, len(sheets))
	tabSpecs := []string{"*"}
	for i, s := range sheets {
		views[i] = sheetView{Name: s.Name, Title: s.Title, GoogleID: s.GoogleID}
		tabSpecs = append(tabSpecs, s.Name+"/*")
	}
	for i, s := range sheets {
		for _, tab := range s.Tabs {
			rule := settings.Spreadsheets[s.Name].Tabs[tab]
			typed := 0
			for _, c := range rule.Columns {
				if c.Type != "" {
					typed++
				}
			}
			views[i].Tabs = append(views[i].Tabs, tabView{tab, rule.Ops, rule.KeyColumn(), typed, len(rule.Hidden)})
			tabSpecs = append(tabSpecs, s.Name+"/"+tab)
		}
	}
	var keys []keyView
	for _, k := range settings.Keys {
		ops := "all the tab allows"
		if len(k.Ops) > 0 {
			ops = strings.Join(k.Ops, ", ")
		}
		keys = append(keys, keyView{k.Name, strings.Join(k.Tabs, ", "), ops})
	}
	return views, keys, tabSpecs, nil
}
