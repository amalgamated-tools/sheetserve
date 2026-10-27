package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
)

// A project ID in GOOGLE_PROJECT_NUMBER fails at startup instead of breaking every pick.
func TestNewRejectsProjectID(t *testing.T) {
	for k, v := range map[string]string{"SHEETSERVE_BASE_URL": "http://localhost:8080",
		"SHEETSERVE_SECRET": base64.StdEncoding.EncodeToString(make([]byte, 32)), "GOOGLE_CLIENT_ID": "123-x",
		"GOOGLE_CLIENT_SECRET": "s", "GOOGLE_PICKER_API_KEY": "k", "GOOGLE_PROJECT_NUMBER": "my-project-512320",
		"SHEETSERVE_DB": t.TempDir() + "/test.db"} {
		t.Setenv(k, v)
	}
	if _, err := newHandler(&config.Config{}); err == nil || !strings.Contains(err.Error(), "GOOGLE_PROJECT_NUMBER") {
		t.Errorf("got %v", err)
	}
}

// Each route reaches its package: sign-in, the per-user API, or the dashboard.
func TestRoutes(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	global := &config.Config{}
	h := routes(global, newApp(global, d, &oauth2.Config{ClientID: "cid",
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth"}}))
	for _, tc := range []struct {
		method, path string
		status       int
		want         string // in the Location header or body
	}{
		{"GET", "/healthz", 200, "ok"},
		{"GET", "/login", 302, "https://accounts.example/auth?"},
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
