package hosted

import (
	"context"
	"net/http"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/auth"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
	"github.com/amalgamated-tools/sheetserve/internal/tenancy"
)

// multiTenant serves the app: users sign in with Google and manage their spreadsheets and keys on
// the dashboard, and tenants serves each user's API to their keys.
type multiTenant struct {
	global    *config.Config // listen, cache_ttl and cors_origins; spreadsheets and keys come from db
	db        *db.DB
	auth      *auth.Google
	pickerKey string // browser API key for Google Picker
	appID     string // Cloud project number, so picked files are granted to this app
	tenants   *tenancy.Tenants
	saEmail   string // the address users share spreadsheets with for tenants.SAStore
}

func newMultiTenant(global *config.Config, d *db.DB, oauth *oauth2.Config) *multiTenant {
	m := &multiTenant{global: global, db: d, auth: auth.New(d, oauth), tenants: &tenancy.Tenants{DB: d, Global: global}}
	m.auth.OnSignIn = m.tenants.Invalidate // pick up the new refresh token
	m.tenants.NewStore = func(userID int64) (store.Store, error) {
		ts, err := m.auth.TokenSource(userID)
		if err != nil {
			return nil, err
		}
		return store.NewGoogleStore(context.Background(), option.WithTokenSource(ts))
	}
	return m
}

func (m *multiTenant) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", m.auth.Login)
	mux.HandleFunc("GET /auth/callback", m.auth.Callback)
	mux.Handle("/v1/", m.tenants)
	mux.Handle("/mcp", m.tenants)
	mux.Handle("GET /openapi.json", m.tenants)
	mux.Handle("/", http.NewCrossOriginProtection().Handler(m.web()))
	return api.CORS(m.global, mux)
}
