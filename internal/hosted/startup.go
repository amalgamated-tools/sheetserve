package hosted

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

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// New sets up the server: Google sign-in, with each user's spreadsheets and keys kept in SQLite.
// If GOOGLE_APPLICATION_CREDENTIALS names a service account key, users can also add spreadsheets
// that the server reaches with that service account.
func New(cfg *config.Config) (http.Handler, error) {
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
	m := newMultiTenant(cfg, d, &oauth2.Config{
		ClientID:     env["GOOGLE_CLIENT_ID"],
		ClientSecret: env["GOOGLE_CLIENT_SECRET"],
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimSuffix(env["SHEETSERVE_BASE_URL"], "/") + "/auth/callback",
		Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"},
	})
	m.pickerKey, m.appID = env["GOOGLE_PICKER_API_KEY"], env["GOOGLE_PROJECT_NUMBER"]
	if path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		if m.saEmail, err = serviceAccountEmail(path); err != nil {
			return nil, err
		}
		if m.saStore, err = store.NewGoogleStore(context.Background()); err != nil {
			return nil, fmt.Errorf("service account: %w", err)
		}
	}
	return m.routes(), nil
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
