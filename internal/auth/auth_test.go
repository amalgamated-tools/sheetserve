package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"golang.org/x/oauth2"

	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/testutil/oteltest"
)

// fakeGoogle returns a Google that signs in through a fake Google, which accepts "good-code" when it
// was issued for redirect.
func fakeGoogle(t *testing.T, d *db.DB, redirect string) *Google {
	t.Helper()
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "good-code" || r.FormValue("code_verifier") == "" || r.FormValue("redirect_uri") != redirect {
				http.Error(w, "bad code", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "refresh_token": "rt-new", "expires_in": 3600})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sub": "123", "email": "carol@example.com"})
		}
	}))
	t.Cleanup(google.Close)
	g := New(d, &oauth2.Config{ClientID: "cid", ClientSecret: "csecret", RedirectURL: redirect,
		Endpoint: oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token"}})
	g.UserinfoURL = google.URL + "/userinfo"
	return g
}

// The OAuth callback checks state, stores the refresh token encrypted, and starts a session.
func TestSignIn(t *testing.T) {
	logs := oteltest.CaptureLogs(t)
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	g := fakeGoogle(t, d, "")
	var signedIn []int64
	g.OnSignIn = func(id int64) { signedIn = append(signedIn, id) }
	mux := chi.NewRouter()
	g.Routes(mux)
	mux.Get("/me", func(w http.ResponseWriter, r *http.Request) {
		if u, ok := g.CurrentUser(r); ok {
			_, _ = io.WriteString(w, u.Email)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(u string) (int, string) {
		t.Helper()
		resp, err := c.Get(srv.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	login := func() url.Values {
		t.Helper()
		resp, err := c.Get(srv.URL + "/login")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		return loc.Query()
	}

	if q := login(); q.Get("access_type") != "offline" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("auth url query: %v", q)
	}
	if s, _ := get("/auth/callback?code=good-code&state=WRONG"); s != 400 {
		t.Errorf("bad state: %d", s)
	}
	state := login().Get("state") // the failed attempt cleared the state cookie
	if s, body := get("/auth/callback?code=good-code&state=" + url.QueryEscape(state)); s != 303 {
		t.Fatalf("callback: %d %s", s, body)
	}
	if _, body := get("/me"); body != "carol@example.com" {
		t.Errorf("current user after sign-in: %q", body)
	}

	var id int64
	var sealed []byte
	if err := d.QueryRow(`SELECT id, refresh_token FROM users WHERE google_sub = '123'`).Scan(&id, &sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "rt-new") {
		t.Error("refresh token stored in plain text")
	}
	if rt, err := d.RefreshToken(context.Background(), id); err != nil || rt != "rt-new" {
		t.Errorf("refresh token: %q %v", rt, err)
	}
	if len(signedIn) != 1 || signedIn[0] != id {
		t.Errorf("OnSignIn calls: %v, want [%d]", signedIn, id)
	}
	if recs := logs("audit"); len(recs) != 1 || recs[0]["action"] != "auth.sign_in" || recs[0]["user_email"] != "carol@example.com" || recs[0]["user_id"] != float64(id) {
		t.Errorf("sign-in audit: %v", recs)
	}

	resp, err := c.Post(srv.URL+"/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, body := get("/me"); body != "" {
		t.Errorf("current user after logout: %q", body)
	}
}

// A preview signs in through another server's relay: its state names the preview, the relay sends
// Google's answer back to the preview's callback, and the relay refuses origins it doesn't list.
func TestRelay(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	relay := New(d, &oauth2.Config{})
	relay.RelayOrigins = []string{"https://nope.example", "http://127.0.0.1:*"}
	relayMux := chi.NewRouter()
	relay.Routes(relayMux)
	relaySrv := httptest.NewServer(relayMux)
	defer relaySrv.Close()

	g := fakeGoogle(t, d, relaySrv.URL+"/auth/relay")
	mux := chi.NewRouter()
	g.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g.Origin = srv.URL

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("redirect_uri") != relaySrv.URL+"/auth/relay" {
		t.Fatalf("redirect_uri: %q", q.Get("redirect_uri"))
	}
	resp, err = c.Get(relaySrv.URL + "/auth/relay?code=good-code&state=" + url.QueryEscape(q.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	next := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(next, srv.URL+"/auth/callback?code=good-code&state=") {
		t.Fatalf("relay: %d %q", resp.StatusCode, next)
	}
	if resp, err = c.Get(next); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Set-Cookie") == "" {
		t.Errorf("callback after relay: %d", resp.StatusCode)
	}

	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:1/x", "http://evil.example?127.0.0.1:1", ""} {
		state := "s." + base64.RawURLEncoding.EncodeToString([]byte(origin))
		resp, err := c.Get(relaySrv.URL + "/auth/relay?code=c&state=" + url.QueryEscape(state))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("relay to %q: %d %q", origin, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// A refresh token Google refuses as expired or revoked is recorded, so the dashboard can say so.
func TestTokenSourceLost(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error": "invalid_grant", "error_description": "Token has been expired or revoked."}`)
	}))
	defer google.Close()
	g := New(d, &oauth2.Config{ClientID: "cid", Endpoint: oauth2.Endpoint{TokenURL: google.URL}})
	ctx := context.Background()
	id, err := d.UpsertUser(ctx, "1", "a@example.com", "rt")
	if err != nil {
		t.Fatal(err)
	}
	ts, err := g.TokenSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(); err == nil {
		t.Fatal("Google refused the token, but Token succeeded")
	}
	if lost, err := d.GoogleLost(ctx, id); err != nil || !lost {
		t.Errorf("lost access not recorded: %v %v", lost, err)
	}
}
