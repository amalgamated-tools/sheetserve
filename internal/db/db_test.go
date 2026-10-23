package db

import (
	"context"
	"database/sql"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/amalgamated-tools/sheetserve/internal/config"
)

// A new database runs every migration, and each one rolls back and reapplies cleanly.
func TestMigrations(t *testing.T) {
	db, err := Open(t.TempDir()+"/new.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsys, _ := fs.Sub(migrations, "migrations")
	p, err := goose.NewProvider(goose.DialectSQLite3, db.DB, fsys)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sources := p.ListSources()
	latest := sources[len(sources)-1].Version
	if v, err := p.GetDBVersion(ctx); err != nil || v != latest {
		t.Fatalf("version after open: %d %v, want %d", v, err, latest)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// A database made before goose, without the service_account column, adopts goose and gains the column.
func TestSchemaUpgrade(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, google_sub TEXT NOT NULL UNIQUE, email TEXT NOT NULL, refresh_token BLOB);
		CREATE TABLE spreadsheets (user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE, name TEXT NOT NULL,
			google_id TEXT NOT NULL, title TEXT NOT NULL, PRIMARY KEY (user_id, name));
		INSERT INTO users (id, google_sub, email) VALUES (1, 's', 's@example.com');
		INSERT INTO spreadsheets VALUES (1, 'shop', 'SS', 'Shop');`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	for range 2 { // the second open must find the column already there
		db, err := Open(path, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := db.UserConfig(context.Background(), 1)
		_ = db.Close()
		if err != nil || cfg.Spreadsheets["shop"].ID != "SS" || cfg.Spreadsheets["shop"].ServiceAccount {
			t.Fatalf("upgraded config: %+v %v", cfg, err)
		}
	}
}

// Dashboard changes land in Spreadsheets in display order, and a change that leaves the config
// invalid is rolled back.
func TestDashboardChanges(t *testing.T) {
	db, err := Open(t.TempDir()+"/dash.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.UpsertUser(ctx, "s", "s@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Spreadsheet{
		{Name: "shop", GoogleID: "SS", Title: "Shop", Tabs: []string{"Orders", "Items"}},
		{Name: "crm", GoogleID: "CC", Title: "CRM", ServiceAccount: true, Tabs: []string{"People"}},
	} {
		if err := db.AddSpreadsheet(ctx, id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveTab(ctx, id, "shop", "Orders", config.TabRule{Ops: []string{"read"}, Hidden: []string{"cost"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTab(ctx, id, "shop", "Nope", config.TabRule{}); err == nil || !strings.Contains(err.Error(), "unknown tab") {
		t.Errorf("save unknown tab: %v", err)
	}
	if err := db.CreateKey(ctx, id, config.Key{Hash: config.HashKey("k"), Name: "bad", Tabs: []string{"shop/Nope"}}); err == nil {
		t.Error("key for an unknown tab was accepted")
	}
	sheets, cfg, err := db.Spreadsheets(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != 2 || sheets[0].Name != "crm" || !sheets[0].ServiceAccount || !slices.Equal(sheets[1].Tabs, []string{"Orders", "Items"}) {
		t.Errorf("spreadsheets: %+v", sheets)
	}
	if rule := cfg.Spreadsheets["shop"].Tabs["Orders"]; !slices.Equal(rule.Ops, []string{"read"}) || rule.KeyColumn() != "id" || len(cfg.Keys) != 0 {
		t.Errorf("config: %+v %+v", rule, cfg.Keys)
	}
	if exists, err := db.SpreadsheetExists(ctx, id, "crm"); err != nil || !exists {
		t.Errorf("crm exists: %v %v", exists, err)
	}
	if err := db.DeleteSpreadsheet(ctx, id, "crm"); err != nil {
		t.Fatal(err)
	}
	if exists, err := db.SpreadsheetExists(ctx, id, "crm"); err != nil || exists {
		t.Errorf("crm after delete: %v %v", exists, err)
	}
}
