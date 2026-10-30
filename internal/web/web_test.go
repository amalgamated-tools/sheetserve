package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
)

type fakeStore struct {
	tabs    map[string][][]any
	created int // Create calls
}

func (f *fakeStore) Create(_ context.Context, title string, tabs []store.Tab) (string, error) {
	f.created++
	id := "new-" + title
	for _, t := range tabs {
		var header []any
		for _, c := range t.Columns {
			header = append(header, c)
		}
		f.tabs[id+"/"+t.Name] = [][]any{header}
	}
	return id, nil
}

func (f *fakeStore) Read(_ context.Context, id, tab string) ([][]any, error) {
	return f.tabs[id+"/"+tab], nil
}

func (f *fakeStore) Append(_ context.Context, id, tab string, row []any) error {
	f.tabs[id+"/"+tab] = append(f.tabs[id+"/"+tab], row)
	return nil
}

func (f *fakeStore) SetCells(_ context.Context, id, tab string, row int, cells map[int]any) error {
	rows := f.tabs[id+"/"+tab]
	if row < 1 || row > len(rows) {
		return fmt.Errorf("row %d out of range", row)
	}
	for c, v := range cells {
		rows[row-1][c] = v
	}
	return nil
}

func (f *fakeStore) DeleteRow(_ context.Context, id, tab string, row int) error {
	key := id + "/" + tab
	f.tabs[key] = slices.Delete(f.tabs[key], row-1, row)
	return nil
}

func (f *fakeStore) Tabs(_ context.Context, id string) ([]string, error) {
	var out []string
	for key := range f.tabs {
		if tab, ok := strings.CutPrefix(key, id+"/"); ok {
			out = append(out, tab)
		}
	}
	slices.Sort(out)
	return out, nil
}

// failingStore fails every call.
type failingStore struct{ err error }

func (e failingStore) Read(context.Context, string, string) ([][]any, error)            { return nil, e.err }
func (e failingStore) Append(context.Context, string, string, []any) error              { return e.err }
func (e failingStore) SetCells(context.Context, string, string, int, map[int]any) error { return e.err }
func (e failingStore) DeleteRow(context.Context, string, string, int) error             { return e.err }
func (e failingStore) Tabs(context.Context, string) ([]string, error)                   { return nil, e.err }

func newTestDashboard(t *testing.T, sheetStore store.Store) (*Dashboard, *httptest.Server) {
	t.Helper()
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	global := &config.Config{}
	tenants := &tenancy.Tenants{DB: d, Global: global,
		NewStore: func(int64) (store.Store, error) { return sheetStore, nil }}
	dash := &Dashboard{DB: d, Auth: auth.New(d, &oauth2.Config{ClientID: "cid", ClientSecret: "csecret"}), Tenants: tenants}
	// The API routes too, so tests can use the keys they create.
	mux := chi.NewRouter()
	mux.Group(func(r chi.Router) {
		r.Use(tenants.Middleware)
		api.Routes(r)
	})
	dash.Routes(mux)
	dash.Auth.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return dash, srv
}

// browser returns a client that keeps cookies and does not follow redirects.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// signInAs signs sub in, approving them as a member unless they are the first user, the admin.
func signInAs(t *testing.T, dash *Dashboard, srv *httptest.Server, sub string) *http.Client {
	t.Helper()
	return signIn(t, dash, srv, sub, true)
}

func signIn(t *testing.T, dash *Dashboard, srv *httptest.Server, sub string, approve bool) *http.Client {
	t.Helper()
	ctx := context.Background()
	id, err := dash.DB.UpsertUser(ctx, sub, sub+"@example.com", "rt-"+sub)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := dash.DB.NewSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u, _, err := dash.DB.SessionUser(ctx, sess); err != nil {
		t.Fatal(err)
	} else if approve && u.Role == rbac.Pending {
		if err := dash.DB.SetRole(ctx, id, rbac.Member); err != nil {
			t.Fatal(err)
		}
	}
	c := browser(t)
	u, _ := url.Parse(srv.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: sess}})
	return c
}

func send(t *testing.T, c *http.Client, req *http.Request) (int, string) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func newRequest(t *testing.T, method, u string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func post(t *testing.T, c *http.Client, u string, form url.Values) (int, string) {
	t.Helper()
	req := newRequest(t, "POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(t, c, req)
}

func get(t *testing.T, c *http.Client, u, key string) (int, string) {
	t.Helper()
	req := newRequest(t, "GET", u, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return send(t, c, req)
}

// A signed-in user adds a sheet, enables a tab, mints a key, and the key reaches the tab;
// settings that would break a key are refused, and a revoked key stops working.
func TestDashboardAndAPI(t *testing.T) {
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Orders": {{"id", "item"}, {"o1", "mug"}},
		"SS/Notes":  {{"id", "text"}},
	}}
	dash, srv := newTestDashboard(t, store)
	alice := signInAs(t, dash, srv, "alice")

	if s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}}); s != 303 {
		t.Fatalf("add sheet: %d %s", s, body)
	}
	if s, body := get(t, alice, srv.URL+"/sheets", ""); s != 200 || !strings.Contains(body, `<a href="https://docs.google.com/spreadsheets/d/SS/edit" target="_blank" rel="noopener"`) {
		t.Errorf("dashboard has no link to the spreadsheet: %d", s)
	}
	if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read"}}); s != 303 {
		t.Fatalf("save tab: %d %s", s, body)
	}
	if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"bogus"}}); s != 400 || !strings.Contains(body, "unknown op") {
		t.Errorf("bad op accepted: %d", s)
	}
	s, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/*"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	if s != 200 || key == "" {
		t.Fatalf("create key: %d %s", s, body)
	}

	c := browser(t)
	if s, body := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 200 || !strings.Contains(body, `"mug"`) {
		t.Errorf("list with key: %d %s", s, body)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Notes", key); s != 403 { // Notes has no ops enabled
		t.Errorf("disabled tab: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Orders", "sk_WRONG"); s != 401 {
		t.Errorf("unknown key: %d", s)
	}
	if s, body := post(t, alice, srv.URL+"/app/sheets/delete", url.Values{"name": {"shop"}}); s != 400 || !strings.Contains(body, "unknown tab") {
		t.Errorf("deleting a sheet a key uses: %d %s", s, body)
	}

	bob := signInAs(t, dash, srv, "bob")
	for _, page := range []string{"/sheets", "/keys"} {
		if _, body := get(t, bob, srv.URL+page, ""); strings.Contains(body, "shop") {
			t.Errorf("bob sees alice's sheet on %s", page)
		}
	}
	if s, _ := post(t, bob, srv.URL+"/app/keys/delete", url.Values{"name": {"web"}}); s != 303 {
		t.Errorf("bob revoke: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 200 {
		t.Errorf("bob revoked alice's key: %d", s)
	}

	if s, _ := post(t, alice, srv.URL+"/app/keys/delete", url.Values{"name": {"web"}}); s != 303 {
		t.Errorf("revoke: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 401 {
		t.Errorf("revoked key still works: %d", s)
	}

	if s, _ := post(t, browser(t), srv.URL+"/app/keys", url.Values{"name": {"x"}, "tabs": {"*"}}); s != 303 {
		t.Errorf("signed-out post: %d", s)
	}
	req := newRequest(t, "POST", srv.URL+"/app/keys", strings.NewReader("name=x&tabs=*"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if s, _ := send(t, alice, req); s != 403 {
		t.Errorf("cross-site post: %d", s)
	}
}

// The dashboard is two pages; / sends a signed-in user to the first, and each form lands back on
// the page it's on.
func TestDashboardPages(t *testing.T) {
	dash, srv := newTestDashboard(t, &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "item"}}}})
	alice := signInAs(t, dash, srv, "alice")
	location := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		req := newRequest(t, method, srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := alice.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}

	if s, loc := location("GET", "/", nil); s != 303 || loc != "/sheets" {
		t.Errorf("signed-in /: %d %q", s, loc)
	}
	if s, body := get(t, browser(t), srv.URL+"/", ""); s != 200 || !strings.Contains(body, "Sign in with Google") {
		t.Errorf("signed-out /: %d", s)
	}
	for _, page := range []string{"/sheets", "/keys"} {
		if s, _ := get(t, browser(t), srv.URL+page, ""); s != 303 {
			t.Errorf("signed-out %s: %d", page, s)
		}
	}
	for _, file := range []string{"/static/app.css", "/static/app.js", "/static/sheets.js"} {
		if s, _ := get(t, browser(t), srv.URL+file, ""); s != 200 {
			t.Errorf("%s: %d", file, s)
		}
	}

	if s, body := get(t, alice, srv.URL+"/sheets", ""); s != 200 || !strings.Contains(body, `<a class="button primary" href="/sheets/new">`) {
		t.Errorf("sheets page has no create button: %d", s)
	}
	s, body := get(t, alice, srv.URL+"/sheets/new", "")
	if s != 200 || !strings.Contains(body, "<title>New spreadsheet · sheetserve</title>") || !strings.Contains(body, `action="/app/sheets/new"`) ||
		!strings.Contains(body, `<a href="/sheets" aria-current="page">`) {
		t.Errorf("new sheet page: %d %s", s, body)
	}
	if s, body := get(t, alice, srv.URL+"/keys", ""); s != 200 || !strings.Contains(body, "Add a spreadsheet</a> first") {
		t.Errorf("keys page with no sheets: %d", s)
	}
	if s, loc := location("POST", "/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}}); s != 303 || loc != "/sheets/columns?sheet=shop" {
		t.Errorf("add sheet: %d %q", s, loc)
	}
	if s, loc := location("POST", "/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read"}}); s != 303 || loc != "/sheets#sheet-shop" {
		t.Errorf("save tab: %d %q", s, loc)
	}
	s, body = get(t, alice, srv.URL+"/sheets", "")
	if s != 200 || !strings.Contains(body, `<title>Spreadsheets · sheetserve</title>`) || !strings.Contains(body, `id="sheet-shop"`) || strings.Contains(body, "New key") {
		t.Errorf("sheets page: %d %s", s, body)
	}

	if s, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}}); s != 400 || !strings.Contains(body, "<title>API keys · sheetserve</title>") {
		t.Errorf("key without tabs shows the keys page: %d", s)
	}
	if s, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/*"}}); s != 200 || !strings.Contains(body, "New key created") {
		t.Errorf("create key: %d", s)
	}
	s, body = get(t, alice, srv.URL+"/keys", "")
	if s != 200 || !strings.Contains(body, "<strong>web</strong>") || strings.Contains(body, "Choose from Google Drive") {
		t.Errorf("keys page: %d %s", s, body)
	}
	if s, loc := location("POST", "/app/keys/delete", url.Values{"name": {"web"}}); s != 303 || loc != "/keys" {
		t.Errorf("revoke: %d %q", s, loc)
	}
	if s, loc := location("POST", "/app/sheets/delete", url.Values{"name": {"shop"}}); s != 303 || loc != "/sheets" {
		t.Errorf("remove sheet: %d %q", s, loc)
	}
}

// When Google rejects a picked spreadsheet, the dashboard shows Google's status and message.
func TestAddSheetShowsGoogleError(t *testing.T) {
	dash, srv := newTestDashboard(t, failingStore{&googleapi.Error{Code: 404, Message: "Requested entity was not found."}})
	alice := signInAs(t, dash, srv, "alice")
	s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}})
	if s != 502 || !strings.Contains(body, "(404: Requested entity was not found.)") {
		t.Errorf("add sheet: %d %s", s, body)
	}
}

// A blank spreadsheet is created from a schema and served like a picked one. A taken name is
// refused before anything is created in Drive, and a bad schema comes back for editing.
func TestCreateSheet(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{}}
	dash, srv := newTestDashboard(t, fs)
	alice := signInAs(t, dash, srv, "alice")

	if s, body := get(t, alice, srv.URL+"/sheets/new", ""); s != 200 || !strings.Contains(body, `<input data-f="tab" name="t0" value=""`) ||
		!strings.Contains(body, `<template id="tab-template">`) || !strings.Contains(body, `src="/static/new-sheet.js"`) {
		t.Errorf("new sheet page: %d %s", s, body)
	}
	// Orders has no key column, so it gets an id. Notes is a blank card, skipped.
	form := url.Values{"name": {"shop"},
		"t0": {"Orders"}, "t0key": {""}, "t0c0": {"item"}, "t0c0type": {"string"}, "t0c0required": {"1"},
		"t0c1": {" "}, "t0c2": {"qty"}, "t0c2type": {"integer"},
		"t1": {""}, "t1c0": {""},
		"t2": {"Products"}, "t2key": {"sku"}, "t2c0": {"sku"}, "t2c0type": {"string"}, "t2c1": {"name"}, "t2c1type": {""},
	}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 303 {
		t.Fatalf("create sheet: %d %s", s, body)
	}
	if got := fs.tabs["new-shop/Orders"]; len(got) == 0 || !slices.Equal(got[0], []any{"id", "item", "qty"}) {
		t.Errorf("Orders header: %v", got)
	}
	if got := fs.tabs["new-shop/Products"]; len(got) == 0 || !slices.Equal(got[0], []any{"sku", "name"}) {
		t.Errorf("Products header: %v", got)
	}
	if _, ok := fs.tabs["new-shop/"]; ok || len(fs.tabs) != 2 {
		t.Errorf("tabs created: %v", slices.Sorted(maps.Keys(fs.tabs)))
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, "key <code>id</code> · 3 typed") || !strings.Contains(body, "key <code>sku</code> · 1 typed") {
		t.Errorf("tab summaries: %s", body)
	}
	if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read", "create"}}); s != 303 {
		t.Fatalf("save tab: %d %s", s, body)
	}
	_, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/Orders"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	for row, want := range map[string]int{`{"item":"mug","qty":2}`: 201, `{"item":"mug","qty":"2"}`: 400, `{"qty":2}`: 400} {
		req := newRequest(t, "POST", srv.URL+"/v1/shop/Orders", strings.NewReader(row))
		req.Header.Set("Authorization", "Bearer "+key)
		if s, body := send(t, browser(t), req); s != want {
			t.Errorf("create row %s: %d %s, want %d", row, s, body, want)
		}
	}

	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 400 || !strings.Contains(body, "already in use") || fs.created != 1 {
		t.Errorf("taken name: %d, %d creates: %s", s, fs.created, body)
	}
	// A failed create shows the form again as it was sent.
	bad := url.Values{"name": {"other"}, "title": {"Other"}, "t0": {"Orders"}, "t0c0": {"a"}, "t0c0type": {"date"}, "t0c1": {"a"}}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", bad); s != 400 || !strings.Contains(body, `column &#34;a&#34; is listed twice`) ||
		!strings.Contains(body, `name="t0" value="Orders"`) || !strings.Contains(body, `name="t0c0" value="a"`) ||
		!strings.Contains(body, `<option value="date" selected>`) || !strings.Contains(body, `name="title" value="Other"`) {
		t.Errorf("bad tabs: %d %s", s, body)
	}
}

// Without a store that can create spreadsheets, the dashboard says so instead of saving a sheet.
func TestCreateSheetUnsupported(t *testing.T) {
	dash, srv := newTestDashboard(t, failingStore{errors.New("unused")})
	alice := signInAs(t, dash, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", url.Values{"name": {"shop"}, "t0": {"Orders"}, "t0c0": {"item"}}); s != 502 || !strings.Contains(body, "could not create") {
		t.Errorf("create sheet: %d %s", s, body)
	}
}

func TestNewTabs(t *testing.T) {
	tabs, rules, err := newTabs([]tabForm{
		{Name: "Orders", Columns: []columnForm{{Name: "item", Type: "string"}}},
		{Name: "People", Columns: []columnForm{{Name: "name"}, {Name: "id", Type: "integer"}}},
		{Name: "Products", Key: "sku", Columns: []columnForm{{Name: "name"}, {Name: "sku", Type: "string"}}},
		{},
	})
	want := []store.Tab{
		{Name: "Orders", Columns: []string{"id", "item"}},
		{Name: "People", Columns: []string{"name", "id"}}, // it lists its own id
		{Name: "Products", Columns: []string{"name", "sku"}},
	}
	if err != nil || !slices.EqualFunc(tabs, want, func(a, b store.Tab) bool { return a.Name == b.Name && slices.Equal(a.Columns, b.Columns) }) {
		t.Errorf("newTabs: %v, %v", tabs, err)
	}
	if r := rules["Orders"]; r.KeyColumn() != "id" || r.Columns["id"].Type != "string" || r.Columns["item"].Type != "string" {
		t.Errorf("Orders rule: %+v", r)
	}
	if r := rules["People"]; r.Columns["id"].Type != "integer" {
		t.Errorf("People rule: %+v", r)
	}
	if r := rules["Products"]; r.KeyColumn() != "sku" {
		t.Errorf("Products rule: %+v", r)
	}
	for msg, forms := range map[string][]tabForm{
		"at least one tab":         nil,
		"give each tab a name":     {{Columns: []columnForm{{Name: "item"}}}},
		"must not contain /":       {{Name: "a/b"}},
		"is listed twice":          {{Name: "Orders"}, {Name: "orders"}},
		"column \"qty\" is listed": {{Name: "Orders", Columns: []columnForm{{Name: "qty"}, {Name: "qty"}}}},
		"not one of its columns":   {{Name: "Orders", Key: "sku", Columns: []columnForm{{Name: "item"}}}},
	} {
		if _, _, err := newTabs(forms); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("newTabs(%v): %v, want %q", forms, err, msg)
		}
	}
}

// Adding a spreadsheet leads to its columns page, which suggests types from each tab's rows. Saved
// types, the key column and hidden columns then shape what the API accepts and returns.
func TestColumns(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{
		"SS/People": {{"email", "name", "age", "vip", "joined", "notes"}, {"ann@example.com", "Ann", 41.0, true, "2024-05-01", ""}},
		"SS/Blank":  {},
	}}
	dash, srv := newTestDashboard(t, fs)
	alice := signInAs(t, dash, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"crm"}, "google_id": {"SS"}, "title": {"CRM"}}); s != 303 {
		t.Fatalf("add sheet: %d %s", s, body)
	}
	s, body := get(t, alice, srv.URL+"/sheets/columns?sheet=crm", "")
	for _, want := range []string{
		"<title>Columns of crm · sheetserve</title>", "Types suggested from its rows", "Row 1 is empty",
		`<input type="hidden" name="t1" value="People">`,
		// email and name are text, age a whole number, vip a checkbox, joined an ISO date; notes is blank.
		`name="t1c0type" aria-label="Type of email"><option value="">Any</option><option value="string" selected>`,
		`name="t1c2type" aria-label="Type of age"><option value="">Any</option><option value="string">Text</option><option value="number">Number</option><option value="integer" selected>`,
		`<option value="boolean" selected>True/false</option>`, `<option value="date" selected>Date</option>`,
		`name="t1c5type" aria-label="Type of notes"><option value="" selected>Any</option>`,
		// People has no id column, so it starts with no key.
		`Row 1 has no <code>id</code> column`, `<input type="radio" name="t1key" value="" checked>`,
	} {
		if s != 200 || !strings.Contains(body, want) {
			t.Errorf("columns page lacks %s: %d %s", want, s, body)
		}
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, `<a href="/sheets/columns?sheet=crm">untyped, set column types</a>`) {
		t.Errorf("untyped tab has no link to its columns: %s", body)
	}
	if s, _ := get(t, alice, srv.URL+"/sheets/columns?sheet=nope", ""); s != 303 {
		t.Errorf("unknown sheet: %d", s)
	}

	people := url.Values{"sheet": {"crm"}, "t0": {"People"}, "t0key": {"email"},
		"t0c0": {"email"}, "t0c0type": {"string"}, "t0c1": {"name"}, "t0c1type": {"string"}, "t0c1required": {"1"},
		"t0c2": {"age"}, "t0c2type": {"integer"}, "t0c3": {"vip"}, "t0c3type": {"boolean"},
		"t0c4": {"joined"}, "t0c4type": {"date"}, "t0c5": {"notes"}, "t0c5hidden": {"1"}}
	bad := url.Values{}
	maps.Copy(bad, people)
	bad.Set("t0key", "nope")
	if s, body := post(t, alice, srv.URL+"/app/sheets/columns", bad); s != 400 || !strings.Contains(body, "not one of its columns") {
		t.Errorf("unknown key column: %d %s", s, body)
	}
	bad.Set("t0key", "notes")
	if s, body := post(t, alice, srv.URL+"/app/sheets/columns", bad); s != 400 || !strings.Contains(body, "can&#39;t be hidden") {
		t.Errorf("hidden key column: %d %s", s, body)
	}
	if s, body := post(t, alice, srv.URL+"/app/sheets/columns", people); s != 303 {
		t.Fatalf("save columns: %d %s", s, body)
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, "key <code>email</code> · 5 typed · 1 hidden") {
		t.Errorf("sheets page summary: %s", body)
	}
	if _, body := get(t, alice, srv.URL+"/sheets/columns?sheet=crm", ""); strings.Contains(body, "Types suggested") ||
		!strings.Contains(body, `name="t1c1required" value="1" checked`) || !strings.Contains(body, `name="t1c5hidden" value="1" checked`) {
		t.Errorf("columns page after save: %s", body)
	}

	post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"crm"}, "tab": {"People"}, "ops": {"read", "create", "update"}})
	_, body = post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"crm/People"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	if s, body := get(t, browser(t), srv.URL+"/v1/crm/People/ann@example.com", key); s != 200 ||
		body != `{"age":41,"email":"ann@example.com","joined":"2024-05-01","name":"Ann","vip":true}`+"\n" {
		t.Errorf("get by email: %d %s", s, body)
	}
	write := func(method, path, body string) (int, string) {
		req := newRequest(t, method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		return send(t, browser(t), req)
	}
	for body, want := range map[string]string{
		`{"email":"bo@example.com","name":"Bo","age":"forty"}`:  `age must be an integer, got \"forty\"`,
		`{"email":"bo@example.com","name":"Bo","age":4.5}`:      `age must be an integer, got 4.5`,
		`{"email":"bo@example.com","name":"Bo","vip":"yes"}`:    `vip must be true or false`,
		`{"email":"bo@example.com","name":"Bo","joined":"5/1"}`: `joined must be a date like 2026-01-31`,
		`{"email":"bo@example.com","name":"Bo","age":""}`:       `age must be an integer, or null to empty it`,
		`{"email":"bo@example.com","name":7}`:                   `name must be a string, got 7`,
		`{"email":"bo@example.com","age":3}`:                    `name is required`,
		`{"email":"bo@example.com","name":"Bo","notes":"x"}`:    `unknown column \"notes\"`,
	} {
		if s, got := write("POST", "/v1/crm/People", body); s != 400 || !strings.Contains(got, want) {
			t.Errorf("create %s: %d %s, want %s", body, s, got, want)
		}
	}
	if s, got := write("POST", "/v1/crm/People", `{"email":"bo@example.com","name":"Bo","age":30,"vip":false,"joined":null}`); s != 201 ||
		got != `{"age":30,"email":"bo@example.com","joined":null,"name":"Bo","vip":false}`+"\n" {
		t.Errorf("create: %d %s", s, got)
	}
	if s, got := write("PATCH", "/v1/crm/People/bo@example.com", `{"name":null}`); s != 400 || !strings.Contains(got, "name is required") {
		t.Errorf("empty a required column: %d %s", s, got)
	}
	if s, got := write("PATCH", "/v1/crm/People/bo@example.com", `{"age":null}`); s != 200 || !strings.Contains(got, `"age":null`) {
		t.Errorf("empty a column: %d %s", s, got)
	}

	_, spec := get(t, browser(t), srv.URL+"/openapi.json", key)
	for _, want := range []string{
		`"age":{"type":["integer","null"]}`, `"joined":{"format":"date","type":["string","null"]}`,
		`"name":{"type":"string"}`, `"email":{"type":"string"}`, `"required":["email","name"]`,
		`"name":"age[gte]","schema":{"type":"integer"}`, `"name":"name[contains]","schema":{"type":"string"}`,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %s: %s", want, spec)
		}
	}
	if strings.Contains(spec, "notes") {
		t.Errorf("spec shows the hidden notes column")
	}
}

// When a typed tab's header changes in Sheets, the columns page marks the new columns, with types
// guessed from their rows, and lists the saved columns row 1 no longer has. Saving drops those.
func TestColumnDrift(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{"SS/People": {{"email", "age", "notes"}, {"ann@example.com", 41.0, "hi"}}}}
	dash, srv := newTestDashboard(t, fs)
	alice := signInAs(t, dash, srv, "alice")
	post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"crm"}, "google_id": {"SS"}, "title": {"CRM"}})
	if s, body := post(t, alice, srv.URL+"/app/sheets/columns", url.Values{"sheet": {"crm"}, "t0": {"People"}, "t0key": {"email"},
		"t0c0": {"email"}, "t0c0type": {"string"}, "t0c1": {"age"}, "t0c1type": {"integer"}, "t0c1required": {"1"}, "t0c2": {"notes"}}); s != 303 {
		t.Fatalf("save columns: %d %s", s, body)
	}

	// Someone renames age to years and adds phone in Sheets.
	fs.tabs["SS/People"] = [][]any{{"email", "years", "notes", "phone"}, {"ann@example.com", 41.0, "hi", "555-0100"}}
	s, body := get(t, alice, srv.URL+"/sheets/columns?sheet=crm", "")
	for _, want := range []string{
		`<strong>years</strong><input type="hidden" name="t0c1" value="years"> <span class="badge new">New</span>`,
		`name="t0c1type" aria-label="Type of years"><option value="">Any</option><option value="string">Text</option><option value="number">Number</option><option value="integer" selected>`,
		`<span class="badge new">New</span>`, `<td><s>age</s></td><td>Integer</td><td>Yes</td>`,
		`<select class="renamed" data-type="integer" data-required data-options="" data-default="" aria-label="New name of age"><option value="">Removed</option> <option value="years">years</option><option value="phone">phone</option></select>`,
		`src="/static/columns.js"`,
	} {
		if s != 200 || !strings.Contains(body, want) {
			t.Errorf("columns page lacks %s: %d %s", want, s, body)
		}
	}
	if strings.Contains(body, `<strong>notes</strong><input type="hidden" name="t0c2" value="notes"> <span class="badge new">`) || strings.Contains(body, "Types suggested") {
		t.Errorf("saved columns marked new: %s", body)
	}

	if s, body := post(t, alice, srv.URL+"/app/sheets/columns", url.Values{"sheet": {"crm"}, "t0": {"People"}, "t0key": {"email"},
		"t0c0": {"email"}, "t0c0type": {"string"}, "t0c1": {"years"}, "t0c1type": {"integer"}, "t0c2": {"notes"}, "t0c3": {"phone"}}); s != 303 {
		t.Fatalf("save columns: %d %s", s, body)
	}
	if _, body := get(t, alice, srv.URL+"/sheets/columns?sheet=crm", ""); strings.Contains(body, "Not in row 1") || strings.Contains(body, "badge new") {
		t.Errorf("drift left after saving: %s", body)
	}
}

// Both column forms take an enum's values and a default, check them, and save them.
func TestEnumAndDefaultForms(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "status", "qty"}, {"o1", "new", 1.0}, {"o2", "paid", 2.0}, {"o3", "new", 1.0}}}}
	dash, srv := newTestDashboard(t, fs)
	alice := signInAs(t, dash, srv, "alice")
	post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}})
	if _, body := get(t, alice, srv.URL+"/sheets/columns?sheet=shop", ""); !strings.Contains(body, `name="t0c1options" value="" data-values="new, paid"`) {
		t.Errorf("columns page offers no values for status: %s", body)
	}
	save := func(extra url.Values) (int, string) {
		form := url.Values{"sheet": {"shop"}, "t0": {"Orders"}, "t0key": {"id"}, "t0c0": {"id"}, "t0c0type": {"string"},
			"t0c1": {"status"}, "t0c1type": {"enum"}, "t0c1options": {" new, paid,new ,"}, "t0c1required": {"1"}, "t0c1default": {"new"},
			"t0c2": {"qty"}, "t0c2type": {"integer"}, "t0c2default": {"1"}}
		maps.Copy(form, extra)
		return post(t, alice, srv.URL+"/app/sheets/columns", form)
	}
	for _, c := range []struct{ field, value, msg string }{
		{"t0c1default", "lost", "status must be one of new, paid"},
		{"t0c2default", "3.5", "qty must be an integer"},
		{"t0c2default", "lots", "the default must be an integer"},
		{"t0c1options", " , ", "list the values an enum allows"},
		{"t0c0default", "x", "the key column id can&#39;t have a default"},
	} {
		if s, body := save(url.Values{c.field: {c.value}}); s != 400 || !strings.Contains(body, c.msg) {
			t.Errorf("%s=%q: %d, want %q", c.field, c.value, s, c.msg)
		}
	}
	if s, body := save(nil); s != 303 {
		t.Fatalf("save: %d %s", s, body)
	}
	_, body := get(t, alice, srv.URL+"/sheets/columns?sheet=shop", "")
	for _, want := range []string{`name="t0c1options" value="new, paid"`, `name="t0c1default" value="new"`, `name="t0c2default" value="1"`, `<option value="enum" selected>One of</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("columns page after save lacks %s", want)
		}
	}

	form := url.Values{"name": {"store"}, "t0": {"Items"}, "t0c0": {"size"}, "t0c0type": {"enum"}, "t0c0options": {"S, M"}, "t0c0default": {"M"}}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 303 {
		t.Fatalf("create: %d %s", s, body)
	}
	if _, body := get(t, alice, srv.URL+"/sheets/columns?sheet=store", ""); !strings.Contains(body, `name="t0c1options" value="S, M"`) || !strings.Contains(body, `name="t0c1default" value="M"`) {
		t.Errorf("created enum not saved: %s", body)
	}
}

func TestGuessType(t *testing.T) {
	for _, c := range []struct {
		vals []any
		want string
	}{
		{nil, ""},
		{[]any{"", nil}, ""},
		{[]any{1.0, 2.0, ""}, "integer"},
		{[]any{1.0, 2.5}, "number"},
		{[]any{true, false}, "boolean"},
		{[]any{"2026-01-31", "2025-12-01"}, "date"},
		{[]any{"2026-01-31T09:30:00Z"}, "datetime"},
		{[]any{"2026-01-31", "soon"}, "string"},
		{[]any{1.0, "one"}, "string"},
		{[]any{"14", 2.0, "-3"}, "integer"}, // numbers stored as text
		{[]any{"14", "2.5"}, "number"},
		{[]any{"0.5"}, "number"},
		{[]any{"02139", "10001"}, "string"}, // ZIP codes keep their zeros
		{[]any{"0", "7"}, "integer"},
	} {
		if got := guessType(c.vals); got != c.want {
			t.Errorf("guessType(%v) = %q, want %q", c.vals, got, c.want)
		}
	}
}

// The first user is admin. Later ones wait as pending, can't change anything, and their keys get
// 403 until approved. Only an admin manages users, and the last admin can't step down.
func TestRoles(t *testing.T) {
	dash, srv := newTestDashboard(t, &fakeStore{})
	alice := signIn(t, dash, srv, "alice", false)
	carol := signIn(t, dash, srv, "carol", false)
	bob := signInAs(t, dash, srv, "bob")
	users, err := dash.DB.Users(context.Background())
	if err != nil || len(users) != 3 || users[0].Role != rbac.Admin || users[1].Role != rbac.Pending || users[2].Role != rbac.Member {
		t.Fatalf("users: %+v %v", users, err)
	}
	carolID := strconv.FormatInt(users[1].ID, 10)

	if s, body := get(t, alice, srv.URL+"/users", ""); s != 200 || !strings.Contains(body, "carol@example.com") {
		t.Errorf("admin users page: %d", s)
	}
	if _, body := get(t, bob, srv.URL+"/sheets", ""); strings.Contains(body, `href="/users"`) {
		t.Error("member sees the users link")
	}
	if s, _ := get(t, bob, srv.URL+"/users", ""); s != 403 {
		t.Errorf("member users page: %d", s)
	}
	if s, _ := post(t, bob, srv.URL+"/app/users/role", url.Values{"id": {carolID}, "role": {"admin"}}); s != 403 {
		t.Errorf("member set role: %d", s)
	}
	for _, path := range []string{"/sheets", "/keys", "/users"} {
		if s, body := get(t, carol, srv.URL+path, ""); s != 403 || !strings.Contains(body, "Waiting for approval") {
			t.Errorf("pending %s: %d", path, s)
		}
	}
	if s, _ := post(t, carol, srv.URL+"/app/keys", url.Values{"name": {"k"}, "tabs": {"*"}}); s != 403 {
		t.Errorf("pending made a key: %d", s)
	}

	setRole := func(id, role string) (int, string) {
		return post(t, alice, srv.URL+"/app/users/role", url.Values{"id": {id}, "role": {role}})
	}
	if s, _ := setRole(carolID, "member"); s != 303 {
		t.Fatalf("approve: %d", s)
	}
	_, body := post(t, carol, srv.URL+"/app/keys", url.Values{"name": {"k"}, "tabs": {"*"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	c := browser(t)
	if s, _ := get(t, c, srv.URL+"/openapi.json", key); s != 200 {
		t.Errorf("approved user's key: %d", s)
	}
	if s, _ := setRole(carolID, "pending"); s != 303 {
		t.Fatalf("unapprove: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/openapi.json", key); s != 403 {
		t.Errorf("pending user's key: %d", s)
	}

	if s, body := setRole(carolID, "owner"); s != 400 || !strings.Contains(body, "unknown role") {
		t.Errorf("unknown role: %d", s)
	}
	if s, body := setRole(strconv.FormatInt(users[0].ID, 10), "member"); s != 400 || !strings.Contains(body, "at least one admin") {
		t.Errorf("last admin stepped down: %d", s)
	}
	if s, _ := post(t, alice, srv.URL+"/app/users/delete", url.Values{"id": {carolID}}); s != 303 {
		t.Errorf("remove: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/openapi.json", key); s != 401 {
		t.Errorf("removed user's key: %d", s)
	}
	if s, _ := get(t, carol, srv.URL+"/sheets", ""); s != 303 {
		t.Errorf("removed user's session: %d", s)
	}
}

// Each dashboard change logs one audit record naming who made it; a refused change logs none.
func TestAuditLog(t *testing.T) {
	logs := oteltest.CaptureLogs(t)
	dash, srv := newTestDashboard(t, &fakeStore{tabs: map[string][][]any{}})
	alice := signInAs(t, dash, srv, "alice") // the first user is the admin
	bob := signInAs(t, dash, srv, "bob")
	users, err := dash.DB.Users(context.Background())
	if err != nil || len(users) != 2 {
		t.Fatalf("users: %+v %v", users, err)
	}
	bobID := strconv.FormatInt(users[1].ID, 10)

	for _, step := range []struct {
		path string
		form url.Values
	}{
		{"/app/sheets/new", url.Values{"name": {"shop"}, "t0": {"Orders"}, "t0c0": {"item"}}},
		{"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read", "create"}}},
		{"/app/sheets/columns", url.Values{"sheet": {"shop"}, "t0": {"Orders"}, "t0key": {"id"},
			"t0c0": {"id"}, "t0c0type": {"string"}, "t0c1": {"item"}, "t0c1type": {"string"}, "t0c1required": {"1"}, "t0c1hidden": {"1"}}},
		{"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/Orders"}, "ops": {"read"}}},
		{"/app/keys/delete", url.Values{"name": {"web"}}},
		{"/app/sheets/delete", url.Values{"name": {"shop"}}},
		{"/app/users/role", url.Values{"id": {bobID}, "role": {"admin"}}},
		{"/app/users/delete", url.Values{"id": {bobID}}},
	} {
		if s, body := post(t, alice, srv.URL+step.path, step.form); s >= 400 {
			t.Fatalf("%s: %d %s", step.path, s, body)
		}
	}
	if s, _ := post(t, bob, srv.URL+"/app/keys", url.Values{"name": {"k"}, "tabs": {"*"}}); s == 200 {
		t.Errorf("removed user made a key: %d", s)
	}

	var actions []string
	for _, rec := range logs("audit") {
		if rec["user_email"] != "alice@example.com" {
			t.Errorf("audit by %v, want alice: %v", rec["user_email"], rec)
		}
		actions = append(actions, rec["action"].(string))
	}
	want := []string{"sheet.create", "tab.save", "tab.columns", "key.create", "key.delete", "sheet.delete", "user.set_role", "user.delete"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit actions: %v, want %v", actions, want)
	}
	if recs := logs("audit"); len(recs) > 3 {
		if tab := recs[1]; tab["sheet"] != "shop" || tab["tab"] != "Orders" || tab["ops"] != "read,create" {
			t.Errorf("tab.save record: %v", tab)
		}
		if tab := recs[2]; tab["tab"] != "Orders" || tab["hidden"] != "item" || tab["key_column"] != "id" || tab["columns"] != "id:string,item:string!" {
			t.Errorf("tab.columns record: %v", tab)
		}
		if key := recs[3]; key["key_name"] != "web" || key["tabs"] != "shop/Orders" || key["ops"] != "read" {
			t.Errorf("key.create record: %v", key)
		}
	}
}

func TestPresetValue(t *testing.T) {
	for ops, want := range map[string]string{
		"":                          "",
		"read":                      "read",
		"update,read,create":        "read,create,update",
		"delete,update,create,read": "read,create,update,delete",
		"read,delete":               "custom",
		"create":                    "custom",
	} {
		var in []string
		if ops != "" {
			in = strings.Split(ops, ",")
		}
		if got := presetValue(in); got != want {
			t.Errorf("presetValue(%q) = %q, want %q", ops, got, want)
		}
	}
}

// Each tab row's preset menu opens on the preset its saved ops match, or on Custom.
func TestOpsPresetMenu(t *testing.T) {
	dash, srv := newTestDashboard(t, &fakeStore{tabs: map[string][][]any{}})
	alice := signInAs(t, dash, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", url.Values{"name": {"shop"}, "t0": {"Orders"}, "t0c0": {"item"}, "t1": {"Notes"}, "t1c0": {"text"}, "t2": {"People"}, "t2c0": {"name"}}); s != 303 {
		t.Fatalf("create sheet: %d %s", s, body)
	}
	for tab, ops := range map[string][]string{"Orders": db.AllOps, "Notes": {"read", "delete"}} {
		if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {tab}, "ops": ops}); s != 303 {
			t.Fatalf("save %s: %d %s", tab, s, body)
		}
	}
	_, body := get(t, alice, srv.URL+"/sheets", "")
	selected := regexp.MustCompile(`aria-label="Ops preset for (\w+)">.*?<option value="([^"]*)"[^>]* selected>`)
	got := map[string]string{}
	for _, m := range selected.FindAllStringSubmatch(body, -1) {
		got[m[1]] = m[2]
	}
	if want := map[string]string{"Orders": "read,create,update,delete", "Notes": "custom", "People": ""}; !maps.Equal(got, want) {
		t.Errorf("selected presets: %v, want %v", got, want)
	}
}

// lostStore fails every call as Google does when a refresh token expired or was revoked, and
// records it as auth's token source would.
type lostStore struct {
	failingStore
	lose func()
}

func (l lostStore) Tabs(context.Context, string) ([]string, error) {
	l.lose()
	return nil, l.err
}

// When Google refuses a user's sign-in, every page says so and offers to reconnect, until a new
// sign-in brings a fresh refresh token.
func TestGoogleLost(t *testing.T) {
	var dash *Dashboard
	var id int64
	lost := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	dash, srv := newTestDashboard(t, lostStore{failingStore{lost}, func() { _ = dash.DB.LoseGoogle(context.Background(), id) }})
	alice := signInAs(t, dash, srv, "alice")
	if err := dash.DB.QueryRow(`SELECT id FROM users WHERE google_sub = 'alice'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); strings.Contains(body, "Reconnect Google") {
		t.Error("banner before Google refused anything")
	}
	s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}})
	if s != 502 || !strings.Contains(body, "Google access expired or was revoked") || !strings.Contains(body, `href="/login">Reconnect Google`) {
		t.Errorf("add sheet with lost access: %d %s", s, body)
	}
	if _, body := get(t, alice, srv.URL+"/keys", ""); !strings.Contains(body, "Reconnect Google") {
		t.Error("the banner should stay on every page")
	}
	if _, err := dash.DB.UpsertUser(context.Background(), "alice", "alice@example.com", "rt-new"); err != nil {
		t.Fatal(err)
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); strings.Contains(body, "Reconnect Google") {
		t.Error("banner after signing in again")
	}
}

// The privacy policy and terms are public, name the operator, and are linked from sign-in.
func TestLegalPages(t *testing.T) {
	dash, srv := newTestDashboard(t, failingStore{errors.New("unused")})
	dash.OperatorName, dash.OperatorContact, dash.BaseURL = "Acme", "privacy@acme.example", "https://sheets.acme.example"
	c := browser(t)
	if s, body := get(t, c, srv.URL+"/privacy", ""); s != 200 || !strings.Contains(body, "run by Acme") ||
		!strings.Contains(body, "Limited Use") || !strings.Contains(body, "Contact Acme at privacy@acme.example.") {
		t.Errorf("privacy: %d %s", s, body)
	}
	if s, body := get(t, c, srv.URL+"/terms", ""); s != 200 || !strings.Contains(body, "https://sheets.acme.example") {
		t.Errorf("terms: %d %s", s, body)
	}
	dash.OperatorName, dash.OperatorContact = "", ""
	if _, body := get(t, c, srv.URL+"/privacy", ""); !strings.Contains(body, "run by the operator of this server") ||
		!strings.Contains(body, "Contact the operator of this server.") {
		t.Errorf("privacy without an operator: %s", body)
	}
	if _, body := get(t, c, srv.URL+"/", ""); !strings.Contains(body, `href="/privacy"`) || !strings.Contains(body, `href="/terms"`) {
		t.Errorf("sign-in page lacks legal links: %s", body)
	}
}
