package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
}
