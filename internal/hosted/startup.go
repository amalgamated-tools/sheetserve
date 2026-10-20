package hosted

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// New sets up the mode used when the config lists no spreadsheets: Google sign-in, with each
// user's spreadsheets and keys kept in SQLite.
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
		return nil, fmt.Errorf("no spreadsheets in the config, so hosted mode needs %v", missing)
	}
	secret, err := base64.StdEncoding.DecodeString(env["SHEETSERVE_SECRET"])
	if err != nil || len(secret) != 32 {
		return nil, errors.New("SHEETSERVE_SECRET must be 32 random bytes, base64 (openssl rand -base64 32)")
	}
	if len(cfg.Keys) > 0 {
		slog.Warn("hosted mode ignores keys in the config file")
	}
	db, err := openDB(cmp.Or(os.Getenv("SHEETSERVE_DB"), "sheetserve.db"), secret)
	if err != nil {
		return nil, err
	}
	m := newMultiTenant(cfg, db, &oauth2.Config{
		ClientID:     env["GOOGLE_CLIENT_ID"],
		ClientSecret: env["GOOGLE_CLIENT_SECRET"],
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimSuffix(env["SHEETSERVE_BASE_URL"], "/") + "/auth/callback",
		Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"},
	})
	m.pickerKey, m.appID = env["GOOGLE_PICKER_API_KEY"], env["GOOGLE_PROJECT_NUMBER"]
	return m.routes(), nil
}
