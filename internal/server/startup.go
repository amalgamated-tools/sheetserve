package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
	"github.com/amalgamated-tools/sheetserve/internal/web"
)

// newHandler sets up the server: Google sign-in, with each user's spreadsheets and keys kept in
// SQLite. If GOOGLE_APPLICATION_CREDENTIALS names a service account key, users can also add
// spreadsheets that the server reaches with that service account.
func newHandler(cfg *config.Config) (http.Handler, error) {
	env := map[string]string{"SHEETSERVE_BASE_URL": os.Getenv("RENDER_EXTERNAL_URL")}
	var missing []string
	for _, k := range []string{"SHEETSERVE_BASE_URL", "SHEETSERVE_SECRET", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET",
		"GOOGLE_PICKER_API_KEY", "GOOGLE_PROJECT_NUMBER"} {
		if env[k] = cmp.Or(os.Getenv(k), env[k]); env[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing environment variables %v", missing)
	}
	// Picker grants drive.file access only when its app ID is the numeric project number.
	if strings.Trim(env["GOOGLE_PROJECT_NUMBER"], "0123456789") != "" {
		return nil, errors.New("GOOGLE_PROJECT_NUMBER must be the numeric project number (the digits before the dash in GOOGLE_CLIENT_ID), not the project ID")
	}
	secret, err := base64.StdEncoding.DecodeString(env["SHEETSERVE_SECRET"])
	if err != nil || len(secret) != 32 {
		return nil, errors.New("SHEETSERVE_SECRET must be 32 random bytes, base64 (openssl rand -base64 32)")
	}
	d, err := db.Open(cmp.Or(os.Getenv("SHEETSERVE_DB"), "sheetserve.db"), secret)
	if err != nil {
		return nil, err
	}
	dash := newApp(cfg, d, &oauth2.Config{
		ClientID:     env["GOOGLE_CLIENT_ID"],
		ClientSecret: env["GOOGLE_CLIENT_SECRET"],
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimSuffix(env["SHEETSERVE_BASE_URL"], "/") + "/auth/callback",
		Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"},
	})
	dash.PickerKey, dash.AppID = env["GOOGLE_PICKER_API_KEY"], env["GOOGLE_PROJECT_NUMBER"]
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		if dash.SAEmail, err = serviceAccountEmail(path); err != nil {
			return nil, err
		}
		if dash.Tenants.SAStore, err = store.NewGoogleStore(context.Background()); err != nil {
			return nil, fmt.Errorf("service account: %w", err)
		}
	}
	return routes(cfg, dash), nil
}

// newApp connects sign-in, each user's API, and the dashboard that edits them.
func newApp(global *config.Config, d *db.DB, oauth *oauth2.Config) *web.Dashboard {
	a := auth.New(d, oauth)
	tenants := &tenancy.Tenants{DB: d, Global: global}
	a.OnSignIn = tenants.Invalidate // pick up the new refresh token
	tenants.NewStore = func(userID int64) (store.Store, error) {
		ts, err := a.TokenSource(userID)
		if err != nil {
			return nil, err
		}
		return store.NewGoogleStore(context.Background(), option.WithTokenSource(ts))
	}
	return &web.Dashboard{DB: d, Auth: a, Tenants: tenants}
}

func routes(global *config.Config, dash *web.Dashboard) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", dash.Auth.Login)
	mux.HandleFunc("GET /auth/callback", dash.Auth.Callback)
	mux.Handle("/v1/", dash.Tenants)
	mux.Handle("/mcp", dash.Tenants)
	mux.Handle("GET /openapi.json", dash.Tenants)
	mux.Handle("/", dash.Handler())
	return api.CORS(global.CORSOrigins, mux)
}

// serviceAccountEmail reads client_email from a service account key file, so the dashboard can
// tell users which address to share their spreadsheets with.
func serviceAccountEmail(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
	var key struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(b, &key); err != nil || key.Type != "service_account" || key.ClientEmail == "" {
		return "", fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: %s is not a service account key file", path)
	}
	return key.ClientEmail, nil
}
