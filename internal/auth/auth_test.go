package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// The OAuth callback checks state, stores the refresh token encrypted, and starts a session.
func TestSignIn(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "good-code" || r.FormValue("code_verifier") == "" {
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
	defer google.Close()
	g := New(d, &oauth2.Config{ClientID: "cid", ClientSecret: "csecret",
		Endpoint: oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token"}})
	g.UserinfoURL = google.URL + "/userinfo"
	var signedIn []int64
	g.OnSignIn = func(id int64) { signedIn = append(signedIn, id) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", g.Login)
	mux.HandleFunc("GET /auth/callback", g.Callback)
	mux.HandleFunc("POST /logout", g.Logout)
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		if _, email, ok := g.CurrentUser(r); ok {
			_, _ = io.WriteString(w, email)
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

	resp, err := c.Post(srv.URL+"/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, body := get("/me"); body != "" {
		t.Errorf("current user after logout: %q", body)
	}
}
