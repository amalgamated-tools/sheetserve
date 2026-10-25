// Package tenancy builds each user's API from their rows in the database and routes API requests
// to it by bearer key.
package tenancy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/amalgamated-tools/sheetserve/internal/api"
	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/service"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// Tenants serves each user's API, reaching Google with that user's credentials, or with the
// server's service account for spreadsheets added that way. Set the fields before first use.
type Tenants struct {
	DB       *db.DB
	Global   *config.Config // listen, cache_ttl and cors_origins; spreadsheets and keys come from DB
	NewStore func(userID int64) (store.Store, error)
	SAStore  store.Store // the server's service account; nil when none is configured

	mu      sync.Mutex
	tenants map[int64]*tenant
	locks   map[int64]*sync.Map // per-user tab write locks; outlive tenant rebuilds
}

type tenant struct {
	h   http.Handler
	svc *service.Service
}

// ServeHTTP hands the request to the API of the user who owns the bearer key.
func (ts *Tenants) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, ok, err := ts.DB.KeyUser(r.Context(), config.HashKey(api.Bearer(r)))
	var t *tenant
	if err == nil && ok {
		t, err = ts.tenant(r.Context(), userID)
	}
	switch {
	case err != nil:
		slog.Error("tenant", "user", userID, "err", err)
		api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream error"})
	case !ok:
		api.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid API key"})
	default:
		t.h.ServeHTTP(w, r)
	}
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
		ts.tenants, ts.locks = map[int64]*tenant{}, map[int64]*sync.Map{}
	}
	cfg, err := ts.DB.UserConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	cfg.Listen, cfg.CacheTTL, cfg.CORSOrigins = ts.Global.Listen, ts.Global.CacheTTL, ts.Global.CORSOrigins
	rs := routedStore{sa: ts.SAStore, viaSA: map[string]bool{}}
	// Without the user's token, their service account spreadsheets still work.
	if rs.user, err = ts.NewStore(userID); err != nil {
		rs.user = errStore{err}
	}
	if rs.sa == nil {
		rs.sa = errStore{errors.New("spreadsheet needs the service account, but the server has none configured")}
	}
	for _, ss := range cfg.Spreadsheets {
		if ss.ServiceAccount {
			rs.viaSA[ss.ID] = true
		}
	}
	svc := service.New(cfg, rs)
	// Requests still running on a replaced tenant hold its tab locks, so every rebuild shares them.
	if ts.locks[userID] == nil {
		ts.locks[userID] = &sync.Map{}
	}
	svc.UseLocks(ts.locks[userID])
	t := &tenant{api.New(cfg, svc).Mux(), svc}
	ts.tenants[userID] = t
	return t, nil
}

// routedStore sends each spreadsheet to the user's credentials or the service account. If a user
// added one spreadsheet both ways, the service account serves both, since it was checked on add.
type routedStore struct {
	user, sa store.Store
	viaSA    map[string]bool // Google spreadsheet ids
}

func (r routedStore) pick(id string) store.Store {
	if r.viaSA[id] {
		return r.sa
	}
	return r.user
}

func (r routedStore) Read(ctx context.Context, id, tab string) ([][]any, error) {
	return r.pick(id).Read(ctx, id, tab)
}

func (r routedStore) Append(ctx context.Context, id, tab string, row []any) error {
	return r.pick(id).Append(ctx, id, tab, row)
}

func (r routedStore) SetCells(ctx context.Context, id, tab string, row int, cells map[int]any) error {
	return r.pick(id).SetCells(ctx, id, tab, row, cells)
}

func (r routedStore) DeleteRow(ctx context.Context, id, tab string, row int) error {
	return r.pick(id).DeleteRow(ctx, id, tab, row)
}

func (r routedStore) Tabs(ctx context.Context, id string) ([]string, error) {
	return r.pick(id).Tabs(ctx, id)
}

// errStore fails every call; it stands in for credentials the server can't use.
type errStore struct{ err error }

func (e errStore) Read(context.Context, string, string) ([][]any, error)            { return nil, e.err }
func (e errStore) Append(context.Context, string, string, []any) error              { return e.err }
func (e errStore) SetCells(context.Context, string, string, int, map[int]any) error { return e.err }
func (e errStore) DeleteRow(context.Context, string, string, int) error             { return e.err }
func (e errStore) Tabs(context.Context, string) ([]string, error)                   { return nil, e.err }
