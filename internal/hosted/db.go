package hosted

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/amalgamated-tools/sheetserve/internal/config"
	"github.com/amalgamated-tools/sheetserve/internal/service"
)

// DB holds the server's state: users, their sessions, and each user's spreadsheets, tabs and keys.
type DB struct {
	*sql.DB
	aead cipher.AEAD // encrypts refresh tokens at rest
}

// ponytail: no migration tool; schema changes need one.
const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	google_sub TEXT NOT NULL UNIQUE,
	email TEXT NOT NULL,
	refresh_token BLOB
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS spreadsheets (
	user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
	name TEXT NOT NULL,
	google_id TEXT NOT NULL,
	title TEXT NOT NULL,
	service_account INTEGER NOT NULL DEFAULT 0, -- 1: reached with the server's service account
	PRIMARY KEY (user_id, name)
);
CREATE TABLE IF NOT EXISTS tabs (
	user_id INTEGER NOT NULL,
	sheet TEXT NOT NULL,
	tab TEXT NOT NULL,
	ops TEXT NOT NULL,    -- JSON array
	hidden TEXT NOT NULL, -- JSON array
	PRIMARY KEY (user_id, sheet, tab),
	FOREIGN KEY (user_id, sheet) REFERENCES spreadsheets ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS api_keys (
	hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users ON DELETE CASCADE,
	name TEXT NOT NULL,
	tabs TEXT NOT NULL, -- JSON array
	ops TEXT NOT NULL,  -- JSON array
	UNIQUE (user_id, name)
);`

// openDB opens (creating if needed) the SQLite file at path. secret is the 32-byte AES key for refresh tokens.
func openDB(path string, secret []byte) (*DB, error) {
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// immediate transactions take the write lock up front, so busy_timeout applies instead of a deadlock error.
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init %s: %w", path, err)
	}
	return &DB{db, aead}, nil
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Databases made before service account support lack this column.
	var has bool
	if err := db.QueryRow(`SELECT count(*) > 0 FROM pragma_table_info('spreadsheets') WHERE name = 'service_account'`).Scan(&has); err != nil || has {
		return err
	}
	_, err := db.Exec(`ALTER TABLE spreadsheets ADD COLUMN service_account INTEGER NOT NULL DEFAULT 0`)
	return err
}

func (d *DB) encrypt(plain string) []byte {
	nonce := make([]byte, d.aead.NonceSize())
	rand.Read(nonce)
	return d.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (d *DB) decrypt(sealed []byte) (string, error) {
	n := d.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("refresh token too short")
	}
	plain, err := d.aead.Open(nil, sealed[:n], sealed[n:], nil)
	return string(plain), err
}

// upsertUser records a Google sign-in. An empty refreshToken keeps the stored one.
func (d *DB) upsertUser(ctx context.Context, sub, email, refreshToken string) (int64, error) {
	var sealed []byte
	if refreshToken != "" {
		sealed = d.encrypt(refreshToken)
	}
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO users (google_sub, email, refresh_token) VALUES (?, ?, ?)
		ON CONFLICT (google_sub) DO UPDATE SET email = excluded.email,
			refresh_token = coalesce(excluded.refresh_token, users.refresh_token)
		RETURNING id`, sub, email, sealed).Scan(&id)
	return id, err
}

func (d *DB) refreshToken(ctx context.Context, userID int64) (string, error) {
	var sealed []byte
	if err := d.QueryRowContext(ctx, `SELECT refresh_token FROM users WHERE id = ?`, userID).Scan(&sealed); err != nil {
		return "", err
	}
	if sealed == nil {
		return "", errors.New("no refresh token; sign in again")
	}
	return d.decrypt(sealed)
}

const sessionTTL = 30 * 24 * time.Hour

// newSession returns a session token for userID; only its hash is stored.
func (d *DB) newSession(ctx context.Context, userID int64) (string, error) {
	tok := rand.Text()
	now := time.Now()
	if _, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
		return "", err
	}
	_, err := d.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		config.HashKey(tok), userID, now.Add(sessionTTL).Unix())
	return tok, err
}

// sessionUser returns the user behind a live session token, or ok=false.
func (d *DB) sessionUser(ctx context.Context, tok string) (id int64, email string, ok bool, err error) {
	err = d.QueryRowContext(ctx, `SELECT u.id, u.email FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, config.HashKey(tok), time.Now().Unix()).Scan(&id, &email)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	return id, email, err == nil, err
}

func (d *DB) deleteSession(ctx context.Context, tok string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, config.HashKey(tok))
	return err
}

// keyUser returns the owner of an API key hash, or ok=false.
func (d *DB) keyUser(ctx context.Context, hash string) (id int64, ok bool, err error) {
	err = d.QueryRowContext(ctx, `SELECT user_id FROM api_keys WHERE hash = ?`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// userConfig builds userID's Config from the database, with the server-wide settings from global.
func userConfig(ctx context.Context, q querier, userID int64, global *config.Config) (*config.Config, error) {
	cfg := &config.Config{Listen: global.Listen, CacheTTL: global.CacheTTL, CORSOrigins: global.CORSOrigins,
		Spreadsheets: map[string]config.Spreadsheet{}}
	err := scan(ctx, q, `SELECT name, google_id, service_account FROM spreadsheets WHERE user_id = ?`, userID, func(rows *sql.Rows) error {
		var name, id string
		var sa bool
		if err := rows.Scan(&name, &id, &sa); err != nil {
			return err
		}
		cfg.Spreadsheets[name] = config.Spreadsheet{ID: id, ServiceAccount: sa, Tabs: map[string]config.TabRule{}}
		return nil
	})
	if err == nil {
		err = scan(ctx, q, `SELECT sheet, tab, ops, hidden FROM tabs WHERE user_id = ?`, userID, func(rows *sql.Rows) error {
			var sheet, tab, ops, hidden string
			var rule config.TabRule
			if err := rows.Scan(&sheet, &tab, &ops, &hidden); err != nil {
				return err
			}
			if err := errors.Join(json.Unmarshal([]byte(ops), &rule.Ops), json.Unmarshal([]byte(hidden), &rule.Hidden)); err != nil {
				return err
			}
			cfg.Spreadsheets[sheet].Tabs[tab] = rule
			return nil
		})
	}
	if err == nil {
		err = scan(ctx, q, `SELECT hash, name, tabs, ops FROM api_keys WHERE user_id = ? ORDER BY name`, userID, func(rows *sql.Rows) error {
			var k config.Key
			var tabs, ops string
			if err := rows.Scan(&k.Hash, &k.Name, &tabs, &ops); err != nil {
				return err
			}
			if err := errors.Join(json.Unmarshal([]byte(tabs), &k.Tabs), json.Unmarshal([]byte(ops), &k.Ops)); err != nil {
				return err
			}
			cfg.Keys = append(cfg.Keys, k)
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, service.BadRequest("%v", err)
	}
	return cfg, nil
}

func scan(ctx context.Context, q querier, query string, userID int64, fn func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, userID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// snapshot runs fn in a read-only transaction, so all its reads see one consistent state.
func (d *DB) snapshot(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// update runs fn in a transaction and commits only if userID's resulting config still validates,
// so validate() checks every dashboard change.
func (d *DB) update(ctx context.Context, userID int64, global *config.Config, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := userConfig(ctx, tx, userID, global); err != nil {
		return err
	}
	return tx.Commit()
}

// jsonList encodes a string list for the JSON columns; nil becomes [].
func jsonList(s []string) string {
	if s == nil {
		return "[]"
	}
	b, _ := json.Marshal(s)
	return string(b)
}
