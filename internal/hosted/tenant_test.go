package hosted

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
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
	for c, v := range cells {
		f.tabs[id+"/"+tab][row-1][c] = v
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

func newTestHosted(t *testing.T, sheetStore store.Store) (*multiTenant, *httptest.Server) {
	t.Helper()
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	global := &config.Config{}
	if err := global.Validate(); err != nil {
		t.Fatal(err)
	}
	m := newMultiTenant(global, d, &oauth2.Config{ClientID: "cid", ClientSecret: "csecret"})
	m.tenants.NewStore = func(int64) (store.Store, error) { return sheetStore, nil }
	srv := httptest.NewServer(m.routes())
	t.Cleanup(srv.Close)
	return m, srv
}

// browser returns a client that keeps cookies and does not follow redirects.
func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func signInAs(t *testing.T, m *multiTenant, srv *httptest.Server, sub string) *http.Client {
	t.Helper()
	ctx := context.Background()
	id, err := m.db.UpsertUser(ctx, sub, sub+"@example.com", "rt-"+sub)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.db.NewSession(ctx, id)
	if err != nil {
		t.Fatal(err)
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

func post(t *testing.T, c *http.Client, u string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(t, c, req)
}

func get(t *testing.T, c *http.Client, u, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return send(t, c, req)
}

// A signed-in user adds a sheet, enables a tab, mints a key, and the key reaches the tab;
// settings that would break a key are refused, and a revoked key stops working.
func TestHostedDashboardAndAPI(t *testing.T) {
	store := &fakeStore{tabs: map[string][][]any{
		"SS/Orders": {{"id", "item"}, {"o1", "mug"}},
		"SS/Notes":  {{"id", "text"}},
	}}
	m, srv := newTestHosted(t, store)
	alice := signInAs(t, m, srv, "alice")

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

	bob := signInAs(t, m, srv, "bob")
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
	req, _ := http.NewRequest("POST", srv.URL+"/app/keys", strings.NewReader("name=x&tabs=*"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if s, _ := send(t, alice, req); s != 403 {
		t.Errorf("cross-site post: %d", s)
	}
}

// The dashboard is two pages; / sends a signed-in user to the first, and each form lands back on
// the page it's on.
func TestDashboardPages(t *testing.T) {
	m, srv := newTestHosted(t, &fakeStore{tabs: map[string][][]any{"SS/Orders": {{"id", "item"}}}})
	alice := signInAs(t, m, srv, "alice")
	location := func(method, path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(form.Encode()))
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

// A spreadsheet added by URL is read with the service account, while picked ones keep using the
// user's token, and the dashboard only offers the URL form when a service account is configured.
func TestServiceAccountSheets(t *testing.T) {
	userStore := &fakeStore{tabs: map[string][][]any{"MINE/Orders": {{"id", "item"}, {"o1", "mug"}}}}
	saStore := &fakeStore{tabs: map[string][][]any{"SHARED/Log": {{"id", "msg"}, {"l1", "hello"}}}}
	m, srv := newTestHosted(t, userStore)
	alice := signInAs(t, m, srv, "alice")

	if _, body := get(t, alice, srv.URL+"/sheets", ""); strings.Contains(body, "Add by URL") {
		t.Error("URL form shown without a service account")
	}
	if s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"via": {"service_account"}, "url": {"SHARED"}, "name": {"log"}}); s != 400 || !strings.Contains(body, "no service account") {
		t.Errorf("add by URL without a service account: %d %s", s, body)
	}

	m.tenants.SAStore, m.saEmail = saStore, "sa@proj.iam.gserviceaccount.com"
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, "sa@proj.iam.gserviceaccount.com") {
		t.Error("URL form does not name the service account")
	}
	if s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"via": {"service_account"}, "url": {"not a url!"}, "name": {"log"}}); s != 400 {
		t.Errorf("bad URL: %d %s", s, body)
	}
	for _, f := range []url.Values{
		{"via": {"service_account"}, "url": {"https://docs.google.com/spreadsheets/d/SHARED/edit#gid=0"}, "name": {"log"}},
		{"name": {"shop"}, "google_id": {"MINE"}, "title": {"Shop"}},
	} {
		if s, body := post(t, alice, srv.URL+"/app/sheets", f); s != 303 {
			t.Fatalf("add sheet %v: %d %s", f, s, body)
		}
	}
	if _, body := get(t, alice, srv.URL+"/sheets", ""); !strings.Contains(body, "via service account") {
		t.Error("dashboard does not mark the service account sheet")
	}
	for _, tab := range [][2]string{{"log", "Log"}, {"shop", "Orders"}} {
		if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {tab[0]}, "tab": {tab[1]}, "ops": {"read"}}); s != 303 {
			t.Fatalf("save tab %v: %d %s", tab, s, body)
		}
	}
	_, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"k"}, "tabs": {"*"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)

	c := browser(t)
	if s, body := get(t, c, srv.URL+"/v1/log/Log", key); s != 200 || !strings.Contains(body, `"hello"`) {
		t.Errorf("service account sheet: %d %s", s, body)
	}
	if s, body := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 200 || !strings.Contains(body, `"mug"`) {
		t.Errorf("user sheet: %d %s", s, body)
	}

	// Without the user's Google token, the service account sheet keeps working.
	m.tenants.NewStore = func(int64) (store.Store, error) { return nil, errors.New("no refresh token") }
	m.tenants.Invalidate(1)
	if s, _ := get(t, c, srv.URL+"/v1/log/Log", key); s != 200 {
		t.Errorf("service account sheet without user token: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 502 {
		t.Errorf("user sheet without user token: %d", s)
	}

	// If the server loses its service account, only those sheets fail.
	m.tenants.NewStore = func(int64) (store.Store, error) { return userStore, nil }
	m.tenants.SAStore = nil
	m.tenants.Invalidate(1)
	if s, _ := get(t, c, srv.URL+"/v1/log/Log", key); s != 502 {
		t.Errorf("service account sheet with no service account: %d", s)
	}
	if s, _ := get(t, c, srv.URL+"/v1/shop/Orders", key); s != 200 {
		t.Errorf("user sheet with no service account: %d", s)
	}
}

func TestSpreadsheetID(t *testing.T) {
	for in, want := range map[string]string{
		"https://docs.google.com/spreadsheets/d/1AbC-d_E/edit#gid=0": "1AbC-d_E",
		" 1AbC-d_E ":           "1AbC-d_E",
		"https://example.com/": "",
		"":                     "",
	} {
		if got := spreadsheetID(in); got != want {
			t.Errorf("spreadsheetID(%q) = %q, want %q", in, got, want)
		}
	}
}

// When Google rejects a picked spreadsheet, the dashboard shows Google's status and message.
func TestAddSheetShowsGoogleError(t *testing.T) {
	m, srv := newTestHosted(t, failingStore{&googleapi.Error{Code: 404, Message: "Requested entity was not found."}})
	alice := signInAs(t, m, srv, "alice")
	s, body := post(t, alice, srv.URL+"/app/sheets", url.Values{"name": {"shop"}, "google_id": {"SS"}, "title": {"Shop"}})
	if s != 502 || !strings.Contains(body, "(404: Requested entity was not found.)") {
		t.Errorf("add sheet: %d %s", s, body)
	}
}

// A project ID in GOOGLE_PROJECT_NUMBER fails at startup instead of breaking every pick.
func TestNewRejectsProjectID(t *testing.T) {
	for k, v := range map[string]string{"SHEETSERVE_BASE_URL": "http://localhost:8080",
		"SHEETSERVE_SECRET": base64.StdEncoding.EncodeToString(make([]byte, 32)), "GOOGLE_CLIENT_ID": "123-x",
		"GOOGLE_CLIENT_SECRET": "s", "GOOGLE_PICKER_API_KEY": "k", "GOOGLE_PROJECT_NUMBER": "my-project-512320",
		"SHEETSERVE_DB": t.TempDir() + "/test.db"} {
		t.Setenv(k, v)
	}
	if _, err := New(&config.Config{}); err == nil || !strings.Contains(err.Error(), "GOOGLE_PROJECT_NUMBER") {
		t.Errorf("got %v", err)
	}
}

// A blank spreadsheet is created from a schema and served like a picked one. A taken name is
// refused before anything is created in Drive, and a bad schema comes back for editing.
func TestCreateSheet(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{}}
	m, srv := newTestHosted(t, fs)
	alice := signInAs(t, m, srv, "alice")

	form := url.Values{"name": {"shop"}, "schema": {"Orders: item, qty\n\nNotes"}}
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", form); s != 303 {
		t.Fatalf("create sheet: %d %s", s, body)
	}
	if got := fs.tabs["new-shop/Orders"]; !slices.Equal(got[0], []any{"id", "item", "qty"}) {
		t.Errorf("Orders header: %v", got)
	}
	if got := fs.tabs["new-shop/Notes"]; !slices.Equal(got[0], []any{"id"}) {
		t.Errorf("Notes header: %v", got)
	}
	if s, body := post(t, alice, srv.URL+"/app/tabs", url.Values{"sheet": {"shop"}, "tab": {"Orders"}, "ops": {"read", "create"}}); s != 303 {
		t.Fatalf("save tab: %d %s", s, body)
	}
	_, body := post(t, alice, srv.URL+"/app/keys", url.Values{"name": {"web"}, "tabs": {"shop/Orders"}})
	key := regexp.MustCompile(`sk_[A-Z0-9]+`).FindString(body)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/shop/Orders", strings.NewReader(`{"item":"mug","qty":2}`))
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
	m, srv := newTestHosted(t, failingStore{errors.New("unused")})
	alice := signInAs(t, m, srv, "alice")
	if s, body := post(t, alice, srv.URL+"/app/sheets/new", url.Values{"name": {"shop"}, "schema": {"Orders: item"}}); s != 502 || !strings.Contains(body, "could not create") {
		t.Errorf("create sheet: %d %s", s, body)
	}
}

func TestParseSchema(t *testing.T) {
	tabs, err := parseSchema(" Orders : item,  qty ,\r\nPeople: id, name\n\n")
	want := []store.Tab{{Name: "Orders", Columns: []string{"id", "item", "qty"}}, {Name: "People", Columns: []string{"id", "name"}}}
	if err != nil || !slices.EqualFunc(tabs, want, func(a, b store.Tab) bool { return a.Name == b.Name && slices.Equal(a.Columns, b.Columns) }) {
		t.Errorf("parseSchema: %v, %v", tabs, err)
	}
	for in, msg := range map[string]string{
		"":                     "at least one tab",
		": item":               "tab name missing",
		"a/b: item":            "must not contain /",
		"Orders\norders: item": "listed twice",
		"Orders: qty, qty":     "listed twice",
	} {
		if _, err := parseSchema(in); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("parseSchema(%q): %v, want %q", in, err, msg)
		}
	}
}

// A tab's key column is checked against its header when saved; left blank, it's id.
func TestSaveTabKey(t *testing.T) {
	fs := &fakeStore{tabs: map[string][][]any{"SS/People": {{"email", "name"}, {"ann@example.com", "Ann"}}}}
	m, srv := newTestHosted(t, fs)
	alice := signInAs(t, m, srv, "alice")
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
