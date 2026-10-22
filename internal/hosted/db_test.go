package hosted

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/amalgamated-tools/sheetserve/internal/config"
)

// A new database runs every migration, and each one rolls back and reapplies cleanly.
func TestMigrations(t *testing.T) {
	db, err := openDB(t.TempDir()+"/new.db", make([]byte, 32))
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
		db, err := openDB(path, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := userConfig(context.Background(), db, 1, &config.Config{})
		_ = db.Close()
		if err != nil || cfg.Spreadsheets["shop"].ID != "SS" || cfg.Spreadsheets["shop"].ServiceAccount {
			t.Fatalf("upgraded config: %+v %v", cfg, err)
		}
	}
}
