package db

import (
	"context"
	"database/sql"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/amalgamated-tools/sheetserve/internal/rbac"
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

// A database made before goose adopts it and runs every migration. Its users keep their access as
// members, and the oldest becomes admin.
func TestSchemaUpgrade(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, google_sub TEXT NOT NULL UNIQUE, email TEXT NOT NULL, refresh_token BLOB);
		CREATE TABLE spreadsheets (user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE, name TEXT NOT NULL,
			google_id TEXT NOT NULL, title TEXT NOT NULL, PRIMARY KEY (user_id, name));
		INSERT INTO users (id, google_sub, email) VALUES (1, 's', 's@example.com'), (2, 't', 't@example.com');
		INSERT INTO spreadsheets VALUES (1, 'shop', 'SS', 'Shop');`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	for range 2 { // the second open must find nothing left to run
		db, err := Open(path, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := db.UserSettings(context.Background(), 1)
		if err != nil || cfg.Spreadsheets["shop"].ID != "SS" {
			t.Fatalf("upgraded config: %+v %v", cfg, err)
		}
		users, err := db.Users(context.Background())
		_ = db.Close()
		if err != nil || len(users) != 2 || users[0].Role != rbac.Admin || users[1].Role != rbac.Member {
			t.Fatalf("upgraded users: %+v %v", users, err)
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
		{Name: "shop", GoogleID: "SS", Title: "Shop", Tabs: []string{"Orders", "Items"},
			Rules: map[string]TabRule{"Items": {Key: "sku", Columns: map[string]Column{"sku": {Type: "string"}, "qty": {Type: "integer", Required: true}}}}},
		{Name: "crm", GoogleID: "CC", Title: "CRM", Tabs: []string{"People"}},
	} {
		if err := db.AddSpreadsheet(ctx, id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveOps(ctx, id, "shop", map[string][]string{"Orders": {"read"}}); err != nil {
		t.Fatal(err)
	}
	// An unknown tab fails the whole save, so Orders keeps read.
	if err := db.SaveOps(ctx, id, "shop", map[string][]string{"Orders": nil, "Nope": nil}); err == nil || !strings.Contains(err.Error(), "unknown tab") {
		t.Errorf("save unknown tab: %v", err)
	}
	if s, err := db.UserSettings(ctx, id); err != nil || len(s.Spreadsheets["shop"].Tabs["Orders"].Ops) != 1 {
		t.Errorf("a failed save changed Orders: %v", err)
	}
	cols := map[string]Column{"qty": {Type: "integer", Required: true, Default: "1"}, "note": {}, "size": {Type: "enum", Options: []string{"S", "M"}}}
	if err := db.SaveColumns(ctx, id, "shop", map[string]TabRule{"Orders": {Hidden: []string{"cost"}, Key: "sku", Columns: cols}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]TabRule{
		{"Nope": {}},
		{"Orders": {Columns: map[string]Column{"qty": {Type: "float"}}}},
		{"Orders": {Columns: map[string]Column{"size": {Type: "enum"}}}},
		{"Orders": {Columns: map[string]Column{"qty": {Type: "integer", Options: []string{"1"}}}}},
	} {
		if err := db.SaveColumns(ctx, id, "shop", bad); err == nil {
			t.Errorf("save columns %v: no error", bad)
		}
	}
	if err := db.CreateKey(ctx, id, Key{Hash: HashKey("k"), Name: "bad", Tabs: []string{"shop/Nope"}}); err == nil {
		t.Error("key for an unknown tab was accepted")
	}
	sheets, cfg, err := db.Spreadsheets(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sheets) != 2 || sheets[0].Name != "crm" || !slices.Equal(sheets[1].Tabs, []string{"Orders", "Items"}) {
		t.Errorf("spreadsheets: %+v", sheets)
	}
	if rule := cfg.Spreadsheets["shop"].Tabs["Orders"]; !slices.Equal(rule.Ops, []string{"read"}) || !slices.Equal(rule.Hidden, []string{"cost"}) ||
		rule.KeyColumn() != "sku" || !reflect.DeepEqual(rule.Columns, cols) || len(cfg.Keys) != 0 {
		t.Errorf("config: %+v %+v", rule, cfg.Keys)
	}
	if rule := cfg.Spreadsheets["shop"].Tabs["Items"]; rule.KeyColumn() != "sku" || !reflect.DeepEqual(rule.Columns["qty"], Column{Type: "integer", Required: true}) {
		t.Errorf("Items as added: %+v", rule)
	}
	if rule := cfg.Spreadsheets["crm"].Tabs["People"]; rule.Columns != nil || rule.KeyColumn() != "id" {
		t.Errorf("untyped tab: %+v", rule)
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

// The first user is admin and later ones pending. A role change or removal that would leave no
// admin is refused.
func TestRoles(t *testing.T) {
	db, err := Open(t.TempDir()+"/roles.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	ids := make([]int64, 0, 3)
	for _, sub := range []string{"a", "b", "a"} { // a signs in twice
		id, err := db.UpsertUser(ctx, sub, sub+"@example.com", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	users, err := db.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Role != rbac.Admin || users[1].Role != rbac.Pending || ids[2] != ids[0] {
		t.Fatalf("users: %+v %v", users, err)
	}
	if err := db.SetRole(ctx, ids[0], rbac.Member); err == nil || !strings.Contains(err.Error(), "at least one admin") {
		t.Errorf("demote the last admin: %v", err)
	}
	if err := db.DeleteUser(ctx, ids[0]); err == nil {
		t.Error("removed the last admin")
	}
	if err := db.SetRole(ctx, 99, rbac.Member); err == nil {
		t.Error("set the role of an unknown user")
	}
	if err := db.SetRole(ctx, ids[1], rbac.Admin); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(ctx, ids[0]); err != nil {
		t.Errorf("remove one of two admins: %v", err)
	}
}

// Lost Google access stays until a sign-in brings a new refresh token.
func TestGoogleLost(t *testing.T) {
	db, err := Open(t.TempDir()+"/lost.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.UpsertUser(ctx, "a", "a@example.com", "rt1")
	if err != nil {
		t.Fatal(err)
	}
	lost := func() bool {
		t.Helper()
		l, err := db.GoogleLost(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if lost() {
		t.Error("lost before any refusal")
	}
	for range 2 { // a second refusal changes nothing
		if err := db.LoseGoogle(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.UpsertUser(ctx, "a", "a@example.com", ""); err != nil || !lost() {
		t.Errorf("a sign-in without a refresh token cleared it: %v", err)
	}
	if _, err := db.UpsertUser(ctx, "a", "a@example.com", "rt2"); err != nil || lost() {
		t.Errorf("a sign-in with a new refresh token kept it: %v", err)
	}
}

// With open sign-up, new users are members at once; the first is still admin, and a returning
// user keeps the role an admin gave them.
func TestOpenSignup(t *testing.T) {
	db, err := Open(t.TempDir()+"/open.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.NewUsers = rbac.Member
	ctx := context.Background()
	for _, sub := range []string{"a", "b"} {
		if _, err := db.UpsertUser(ctx, sub, sub+"@example.com", ""); err != nil {
			t.Fatal(err)
		}
	}
	users, err := db.Users(ctx)
	if err != nil || len(users) != 2 || users[0].Role != rbac.Admin || users[1].Role != rbac.Member {
		t.Fatalf("users: %+v %v", users, err)
	}
	if err := db.SetRole(ctx, users[1].ID, rbac.Pending); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertUser(ctx, "b", "b@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if users, err := db.Users(ctx); err != nil || len(users) != 2 || users[1].Role != rbac.Pending {
		t.Errorf("a suspended user signing in again: %+v %v", users, err)
	}
}

// A key records when it was made, and when it was last used, at most once a minute.
func TestKeyTimes(t *testing.T) {
	db, err := Open(t.TempDir()+"/keys.db", make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id, err := db.UpsertUser(ctx, "a", "a@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateKey(ctx, id, Key{Name: "k", Hash: HashKey("k"), Tabs: []string{"*"}}); err != nil {
		t.Fatal(err)
	}
	key := func() Key {
		t.Helper()
		s, err := db.UserSettings(ctx, id)
		if err != nil || len(s.Keys) != 1 {
			t.Fatalf("keys: %v", err)
		}
		return s.Keys[0]
	}
	if k := key(); time.Since(k.Created) > time.Minute || !k.LastUsed.IsZero() {
		t.Errorf("new key: %+v", k)
	}
	if err := db.TouchKey(ctx, HashKey("k")); err != nil {
		t.Fatal(err)
	}
	used := key().LastUsed
	if time.Since(used) > time.Minute {
		t.Errorf("last used: %v", used)
	}
	if _, err := db.Exec(`UPDATE api_keys SET last_used_at = last_used_at - 30`); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchKey(ctx, HashKey("k")); err != nil || !key().LastUsed.Equal(used.Add(-30*time.Second)) {
		t.Errorf("a use within a minute wrote again: %v", err)
	}
}
