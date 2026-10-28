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
	if s, loc := location("POST", "/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}}); s != 303 || loc != "/sheets#sheet-shop" {
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

	form := url.Values{"name": {"shop"}, "schema": {"Orders: item, qty\n\nNotes\nProducts: *sku, name"}}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 303 {
		t.Fatalf("create sheet: %d %s", s, body)
	}
	if got := fs.tabs["new-shop/Orders"]; len(got) == 0 || !slices.Equal(got[0], []any{"id", "item", "qty"}) {
		t.Errorf("Orders header: %v", got)
	}
	if got := fs.tabs["new-shop/Notes"]; len(got) == 0 || !slices.Equal(got[0], []any{"id"}) {
		t.Errorf("Notes header: %v", got)
	}
	// A tab with a * key gets no id column, and its key column is set already.
	if got := fs.tabs["new-shop/Products"]; len(got) == 0 || !slices.Equal(got[0], []any{"sku", "name"}) {
		t.Errorf("Products header: %v", got)
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, `value="sku"`) {
		t.Errorf("Products key column not shown as sku: %s", body)
	}
	if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read", "create"}}); s != 303 {
		t.Fatalf("save tab: %d %s", s, body)
	}
	_, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/Orders"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	req := newRequest(t, "POST", srv.URL+"/v1/shop/Orders", strings.NewReader(`{"item":"mug","qty":2}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if s, body := send(t, browser(t), req); s != 201 {
		t.Errorf("create row: %d %s", s, body)
	}

	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 400 || !strings.Contains(body, "already in use") || fs.created != 1 {
		t.Errorf("taken name: %d, %d creates: %s", s, fs.created, body)
	}
	bad := url.Values{"name": {"other"}, "schema": {"Orders: a, a"}}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", bad); s != 400 || !strings.Contains(body, `column &#34;a&#34; is listed twice`) || !strings.Contains(body, ">Orders: a, a</textarea>") ||
		!strings.Contains(body, "<title>New spreadsheet · sheetserve</title>") {
		t.Errorf("bad schema: %d %s", s, body)
	}
}

// Without a store that can create spreadsheets, the dashboard says so instead of saving a sheet.
func TestCreateSheetUnsupported(t *testing.T) {
	dash, srv := newTestDashboard(t, failingStore{errors.New("unused")})
	alice := signInAs(t, dash, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", url.Values{"name": {"shop"}, "schema": {"Orders: item"}}); s != 502 || !strings.Contains(body, "could not create") {
		t.Errorf("create sheet: %d %s", s, body)
	}
}

func TestParseSchema(t *testing.T) {
	tabs, keys, err := parseSchema(" Orders : item,  qty ,\r\nPeople: id, name\nProducts: name, * sku\n\n")
	want := []store.Tab{
		{Name: "Orders", Columns: []string{"id", "item", "qty"}},
		{Name: "People", Columns: []string{"id", "name"}},
		{Name: "Products", Columns: []string{"name", "sku"}},
	}
	if err != nil || !slices.EqualFunc(tabs, want, func(a, b store.Tab) bool { return a.Name == b.Name && slices.Equal(a.Columns, b.Columns) }) {
		t.Errorf("parseSchema: %v, %v", tabs, err)
	}
	if !maps.Equal(keys, map[string]string{"Products": "sku"}) {
		t.Errorf("parseSchema keys: %v", keys)
	}
	for in, msg := range map[string]string{
		"":                     "at least one tab",
		": item":               "tab name missing",
		"a/b: item":            "must not contain /",
		"Orders\norders: item": "listed twice",
		"Orders: qty, qty":     "listed twice",
		"Orders: *sku, sku":    "listed twice",
		"Orders: *a, *b":       "only one column",
		"Orders: item, *":      "needs a column name",
	} {
		if _, _, err := parseSchema(in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("parseSchema(%q): %v, want %q", in, err, msg)
		}
	}
}

// A tab's key column is checked against its header when saved; left blank, it's id.
func TestSaveTabKey(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{"SS/People": {{"email", "name"}, {"ann@example.com", "Ann"}}}}
	dash, srv := newTestDashboard(t, fs)
	alice := signInAs(t, dash, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"crm"}, "google_id": {"SS"}, "title": {"CRM"}}); s != 303 {
		t.Fatalf("add sheet: %d %s", s, body)
	}
	save := func(key string) (int, string) {
		return post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"crm"}, "tab": {"People"}, "ops": {"read"}, "key": {key}})
	}
	if s, body := save("nope"); s != 400 || !strings.Contains(body, `crm/People has no &#34;nope&#34; column in row 1`) {
		t.Errorf("unknown key: %d %s", s, body)
	}
	if s, body := save(" email "); s != 303 {
		t.Fatalf("save key: %d %s", s, body)
	}
	_, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"crm/People"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	if s, body := get(t, browser(t), srv.URL+"/v1/crm/People/ann@example.com", key); s != 200 || !strings.Contains(body, `"Ann"`) {
		t.Errorf("get by email: %d %s", s, body)
	}
	if s, body := get(t, alice, srv.URL+"/sheets", ""); s != 200 || !strings.Contains(body, `name="key" value="email"`) {
		t.Errorf("dashboard key: %d", s)
	}
	if s, body := save(""); s != 303 {
		t.Fatalf("clear key: %d %s", s, body)
	}
	if s, body := get(t, browser(t), srv.URL+"/v1/crm/People/ann@example.com", key); s != 403 || !strings.Contains(body, "can only be listed") {
		t.Errorf("get without id column: %d %s", s, body)
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
		{"/app/sheets/new", url.Values{"name": {"shop"}, "schema": {"Orders: item"}}},
		{"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read", "create"}, "hidden": {"item"}}},
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
	want := []string{"sheet.create", "tab.save", "key.create", "key.delete", "sheet.delete", "user.set_role", "user.delete"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit actions: %v, want %v", actions, want)
	}
	if recs := logs("audit"); len(recs) > 2 {
		if tab := recs[1]; tab["sheet"] != "shop" || tab["tab"] != "Orders" || tab["ops"] != "read,create" || tab["hidden"] != "item" || tab["key_column"] != "id" {
			t.Errorf("tab.save record: %v", tab)
		}
		if key := recs[2]; key["key_name"] != "web" || key["tabs"] != "shop/Orders" || key["ops"] != "read" {
			t.Errorf("key.create record: %v", key)
		}
	}
}
