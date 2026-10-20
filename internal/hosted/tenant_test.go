package sheetserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func newTestHosted(t *testing.T, store Store) (*multiTenant, *httptest.Server) {
	t.Helper()
	db, err := openDB(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	global := &Config{}
	if err := global.validate(); err != nil {
		t.Fatal(err)
	}
	m := newMultiTenant(global, db, &oauth2.Config{ClientID: "cid", ClientSecret: "csecret"})
	m.newStore = func(int64) (Store, error) { return store, nil }
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
	id, err := m.db.upsertUser(ctx, sub, sub+"@example.com", "rt-"+sub)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := m.db.newSession(ctx, id)
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
	defer resp.Body.Close()
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
	if _, body := get(t, bob, srv.URL+"/", ""); strings.Contains(body, "shop") {
		t.Error("bob sees alice's sheet")
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

// The OAuth callback checks state, stores the refresh token encrypted, and starts a session.
func TestHostedSignIn(t *testing.T) {
	m, srv := newTestHosted(t, &fakeStore{})
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "good-code" || r.FormValue("code_verifier") == "" {
				http.Error(w, "bad code", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "refresh_token": "rt-new", "expires_in": 3600})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at" {
				http.Error(w, "no token", 401)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"sub": "123", "email": "carol@example.com"})
		}
	}))
	defer google.Close()
	m.oauth.Endpoint = oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token"}
	m.userinfoURL = google.URL + "/userinfo"

	c := browser(t)
	resp, err := c.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if q := loc.Query(); q.Get("access_type") != "offline" || q.Get("code_challenge") == "" {
		t.Fatalf("auth url: %s", loc)
	}
	state := loc.Query().Get("state")

	if s, _ := get(t, c, srv.URL+"/auth/callback?code=good-code&state=WRONG", ""); s != 400 {
		t.Errorf("bad state: %d", s)
	}
	resp, _ = c.Get(srv.URL + "/login") // the failed attempt cleared the state cookie
	state = must(url.Parse(resp.Header.Get("Location"))).Query().Get("state")
	if s, body := get(t, c, srv.URL+"/auth/callback?code=good-code&state="+url.QueryEscape(state), ""); s != 303 {
		t.Fatalf("callback: %d %s", s, body)
	}
	if _, body := get(t, c, srv.URL+"/", ""); !strings.Contains(body, "carol@example.com") {
		t.Errorf("dashboard after sign-in: %s", body)
	}

	var id int64
	var sealed []byte
	if err := m.db.QueryRow(`SELECT id, refresh_token FROM users WHERE google_sub = '123'`).Scan(&id, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "rt-new") {
		t.Error("refresh token stored in plain text")
	}
	if rt, err := m.db.refreshToken(context.Background(), id); err != nil || rt != "rt-new" {
		t.Errorf("refresh token: %q %v", rt, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A rebuilt tenant shares tab write locks with the one it replaced, which may still be serving requests.
func TestTenantRebuildKeepsLocks(t *testing.T) {
	m, _ := newTestHosted(t, &fakeStore{})
	id, err := m.db.upsertUser(context.Background(), "dave", "dave@example.com", "rt")
	if err != nil {
		t.Fatal(err)
	}
	old, err := m.tenant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	m.invalidate(id)
	fresh, err := m.tenant(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if old == fresh || old.svc.locks != fresh.svc.locks {
		t.Fatal("rebuilt tenant has its own tab locks")
	}
}
