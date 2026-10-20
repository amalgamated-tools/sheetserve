// Command sheetserve serves Google Sheets tabs as a REST API and an MCP endpoint.
package sheetserve

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const version = "0.1.0"

func Main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		key := newKey()
		fmt.Printf("key:  %s   (give this to the client; it is not stored)\nhash: %s   (put this in config.yaml)\n", key, hashKey(key))
		return
	}
	path := flag.String("config", cmp.Or(os.Getenv("SHEETSERVE_CONFIG"), "config.yaml"), "path to config file")
	flag.Parse()

	if err := run(*path); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	var h http.Handler
	if len(cfg.Spreadsheets) == 0 {
		if h, err = hosted(cfg); err != nil {
			return err
		}
	} else {
		store, err := newGoogleStore(context.Background())
		if err != nil {
			return fmt.Errorf("google sheets client: %w", err)
		}
		h = (&API{cfg: cfg, svc: newService(cfg, store)}).routes()
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	slog.Info("listening", "addr", cfg.Listen, "version", version, "hosted", len(cfg.Spreadsheets) == 0)
	return srv.ListenAndServe()
}

// hosted sets up the mode used when the config lists no spreadsheets: Google sign-in, with each
// user's spreadsheets and keys kept in SQLite.
func hosted(cfg *Config) (http.Handler, error) {
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
