package tenancy

import (
	"context"
	"errors"
	"testing"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/db"
	"github.com/amalgamated-tools/sheetserve/internal/store"
)

// A rebuilt tenant shares the cache and tab write locks with the one it replaced, which may still be
// serving requests.
func TestRebuildKeepsLocks(t *testing.T) {
	d, err := db.Open(t.TempDir()+"/test.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ts := &Tenants{DB: d, Global: &config.Config{},
		NewStore: func(int64) (store.Store, error) { return errStore{errors.New("unused")}, nil }}
	ctx := context.Background()
	id, err := d.UpsertUser(ctx, "dave", "dave@example.com", "rt")
	if err != nil {
		t.Fatal(err)
	}
	old, err := ts.Service(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ts.Invalidate(id)
	fresh, err := ts.Service(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if old == fresh || !old.SharesCache(fresh) {
		t.Fatal("rebuilt tenant has its own cache and tab locks")
	}
}
