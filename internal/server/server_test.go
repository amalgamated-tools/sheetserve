package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// Each route reaches its package: sign-in, the per-user API, or the dashboard.
func TestRoutes(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s, err := New(
		t.Context(),
		WithConfig(&config.Config{GoogleClientID: "cid"}),
		WithDB(d),
	)
	if err != nil {
		t.Fatal(err)
	}
	h := s.router
	for _, tc := range []struct {
		method, path string
		status       int
		want         string // in the Location header or body
	}{
		{"GET", "/healthz", 200, "."},
		{"GET", "/login", 302, "https://accounts.google.com/o/oauth2/auth?"},
		{"GET", "/auth/callback", 400, "invalid sign-in state"},
		{"GET", "/v1/shop/Orders", 401, "missing or invalid API key"},
		{"POST", "/mcp", 401, "missing or invalid API key"},
		{"GET", "/openapi.json", 401, "missing or invalid API key"},
		{"GET", "/", 200, "Sign in with Google"},
		{"GET", "/sheets", 303, "/"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		got := rec.Header().Get("Location") + rec.Body.String()
		if rec.Code != tc.status || !strings.Contains(got, tc.want) {
			t.Errorf("%s %s: %d %q, want %d containing %q", tc.method, tc.path, rec.Code, got, tc.status, tc.want)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/app/keys", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site dashboard post: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site logout: %d", rec.Code)
	}
}

// The server's routes, from every package, live in one tree. /debug is left out; /mcp takes any method.
func TestRouteTable(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s, err := New(t.Context(), WithConfig(&config.Config{}), WithDB(d))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		switch {
		case strings.HasPrefix(route, "/debug"):
		case route == "/mcp":
			if method == "POST" {
				got = append(got, "ANY /mcp")
			}
		default:
			got = append(got, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{
		"ANY /mcp",
		"DELETE /v1/{sheet}/{tab}/{id}",
		"GET /",
		"GET /app/token",
		"GET /auth/callback",
		"GET /auth/relay",
		"GET /keys",
		"GET /login",
		"GET /openapi.json",
		"GET /sheets",
		"GET /sheets/columns",
		"GET /sheets/new",
		"GET /static/*",
		"GET /users",
		"GET /v1/{sheet}/{tab}",
		"GET /v1/{sheet}/{tab}/{id}",
		"PATCH /v1/{sheet}/{tab}/{id}",
		"POST /app/keys",
		"POST /app/keys/delete",
		"POST /app/sheets",
		"POST /app/sheets/columns",
		"POST /app/sheets/delete",
		"POST /app/sheets/new",
		"POST /app/tabs",
		"POST /app/users/delete",
		"POST /app/users/role",
		"POST /logout",
		"POST /v1/{sheet}/{tab}",
	}
	if !slices.Equal(got, want) {
		t.Errorf("routes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A server signs in through SHEETSERVE_AUTH_RELAY unless it is the relay, and names itself in the state.
func TestAuthRelay(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, tc := range []struct{ base, redirect, origin string }{
		{"https://sheets.example", "https://sheets.example/auth/callback", ""},
		{"https://sheets-pr-4.example/", "https://sheets.example/auth/relay", "https://sheets-pr-4.example"},
	} {
		s, err := New(t.Context(), WithDB(d), WithConfig(&config.Config{BaseURL: tc.base, AuthRelay: "https://sheets.example/auth/relay"}))
		if err != nil {
			t.Fatal(err)
		}
		if s.auth.OAuth.RedirectURL != tc.redirect || s.auth.Origin != tc.origin {
			t.Errorf("%s: redirect %q, origin %q", tc.base, s.auth.OAuth.RedirectURL, s.auth.Origin)
		}
	}
}
