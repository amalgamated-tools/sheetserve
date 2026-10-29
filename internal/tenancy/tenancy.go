// Package tenancy builds each user's API from their rows in the database and picks it for each API
// request by bearer key.
package tenancy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/rbac"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// Tenants serves each user's API, reaching Google with that user's credentials. Set the fields
// before first use.
type Tenants struct {
	DB       *db.DB
	Global   *config.Config // server-wide settings; each user's spreadsheets and keys come from DB
	NewStore func(userID int64) (store.Store, error)

	mu      sync.Mutex
	tenants map[int64]*tenant
	cache   *service.Cache // every user's tabs and tab locks; outlives tenant rebuilds
}

type tenant struct {
	api *api.API
	svc *service.Service
}

// Middleware serves each request with the API of the user who owns the bearer key, if their role
// lets their keys work. Put api.Routes behind it.
func (ts *Tenants) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, role, ok, err := ts.DB.KeyUser(r.Context(), db.HashKey(api.Bearer(r)))
		var t *tenant
		if err == nil && ok && role.Can(rbac.UseAPI) {
			t, err = ts.tenant(r.Context(), userID)
		}
		switch {
		case err != nil:
			slog.Error("tenant", "user", userID, "err", err)
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream error"})
		case !ok:
			api.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"})
		case !role.Can(rbac.UseAPI):
			api.WriteJSON(w, http.StatusForbidden, map[string]string{"error": "the key's owner is not approved on this server"})
		default:
			next.ServeHTTP(w, r.WithContext(api.NewContext(r.Context(), t.api)))
		}
	})
}

// Service returns userID's service, built from their current settings.
func (ts *Tenants) Service(ctx context.Context, userID int64) (*service.Service, error) {
	t, err := ts.tenant(ctx, userID)
	if err != nil {
		return nil, err
	}
	return t.svc, nil
}

// Invalidate drops userID's cached API so the next request sees their latest settings and token.
// Tabs already read stay cached.
func (ts *Tenants) Invalidate(userID int64) {
	ts.mu.Lock()
	delete(ts.tenants, userID)
	ts.mu.Unlock()
}

// TODO: tenants are cached in-process; a second replica needs shared invalidation.
func (ts *Tenants) tenant(ctx context.Context, userID int64) (*tenant, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.tenants[userID]; ok {
		return t, nil
	}
	if ts.tenants == nil {
		ts.tenants, ts.cache = map[int64]*tenant{}, service.NewCache(ts.Global.CacheTTL)
	}
	settings, err := ts.DB.UserSettings(ctx, userID)
	if err != nil {
		return nil, err
	}
	st, err := ts.NewStore(userID)
	if err != nil {
		st = errStore{err} // each call fails until the user signs in again
	}
	svc := service.New(settings, st, ts.Global.CacheTTL)
	// Requests still running on a replaced tenant hold its tab locks, so every rebuild shares them,
	// and keeps the tabs already read.
	svc.UseCache(ts.cache)
	t := &tenant{api.New(settings, svc), svc}
	ts.tenants[userID] = t
	return t, nil
}

// errStore fails every call; it stands in for a user's missing Google credentials.
type errStore struct{ err error }

func (e errStore) Read(context.Context, string, string) ([][]any, error)            { return nil, e.err }
func (e errStore) Append(context.Context, string, string, []any) error              { return e.err }
func (e errStore) SetCells(context.Context, string, string, int, map[int]any) error { return e.err }
func (e errStore) DeleteRow(context.Context, string, string, int) error             { return e.err }
func (e errStore) Tabs(context.Context, string) ([]string, error)                   { return nil, e.err }
