package db

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/amalgamated-tools/sheetserve/internal/rbac"
)

// DB holds the server's state: users, their sessions, and each user's spreadsheets, tabs and keys.
type DB struct {
	*sql.DB
	aead cipher.AEAD // encrypts refresh tokens at rest
}

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens (creating if needed) the SQLite file at path. secret is the 32-byte AES key for refresh tokens.
func Open(path string, secret []byte) (*DB, error) {
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
	if err := migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init %s: %w", path, err)
	}
	return &DB{db, aead}, nil
}

// migrate applies any migrations in migrations/ that db has not run yet. Add one with `mise run db:new <name>`.
func migrate(ctx context.Context, db *sql.DB) error {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
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

// User is someone who has signed in.
type User struct {
	ID    int64
	Email string
	Role  rbac.Role
}

// UpsertUser records a Google sign-in. An empty refreshToken keeps the stored one. The server's
// first user becomes admin; later ones wait as pending until an admin approves them.
func (d *DB) UpsertUser(ctx context.Context, sub, email, refreshToken string) (int64, error) {
	var sealed []byte
	if refreshToken != "" {
		sealed = d.encrypt(refreshToken)
	}
	var id int64
	err := d.QueryRowContext(ctx, `INSERT INTO users (google_sub, email, refresh_token, role)
		VALUES (?, ?, ?, CASE WHEN EXISTS (SELECT 1 FROM users) THEN ? ELSE ? END)
		ON CONFLICT (google_sub) DO UPDATE SET email = excluded.email,
			refresh_token = coalesce(excluded.refresh_token, users.refresh_token)
		RETURNING id`, sub, email, sealed, rbac.Pending, rbac.Admin).Scan(&id)
	return id, err
}

// Users returns everyone who has signed in, oldest first.
func (d *DB) Users(ctx context.Context) ([]User, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, email, role FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetRole gives userID role, unless that would leave the server without an admin.
func (d *DB) SetRole(ctx context.Context, userID int64, role rbac.Role) error {
	if !role.Valid() {
		return ValidationError("unknown role")
	}
	return d.keepAdmin(ctx, func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID)
	})
}

// DeleteUser removes userID with their sessions, spreadsheets and keys, unless they are the last admin.
func (d *DB) DeleteUser(ctx context.Context, userID int64) error {
	return d.keepAdmin(ctx, func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	})
}

// keepAdmin runs a change to one user in a transaction and commits it only if an admin remains.
func (d *DB) keepAdmin(ctx context.Context, change func(tx *sql.Tx) (sql.Result, error)) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := change(tx)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, error(ValidationError("unknown user")))
	}
	var admins int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = ?`, rbac.Admin).Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		return ValidationError("the server needs at least one admin")
	}
	return tx.Commit()
}

func (d *DB) RefreshToken(ctx context.Context, userID int64) (string, error) {
	var sealed []byte
	if err := d.QueryRowContext(ctx, `SELECT refresh_token FROM users WHERE id = ?`, userID).Scan(&sealed); err != nil {
		return "", err
	}
	if sealed == nil {
		return "", errors.New("no refresh token; sign in again")
	}
	return d.decrypt(sealed)
}

const SessionTTL = 30 * 24 * time.Hour

// NewSession returns a session token for userID; only its hash is stored.
func (d *DB) NewSession(ctx context.Context, userID int64) (string, error) {
	tok := rand.Text()
	now := time.Now()
	if _, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
		return "", err
	}
	_, err := d.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		HashKey(tok), userID, now.Add(SessionTTL).Unix())
	return tok, err
}

// SessionUser returns the user behind a live session token, or ok=false.
func (d *DB) SessionUser(ctx context.Context, tok string) (u User, ok bool, err error) {
	err = d.QueryRowContext(ctx, `SELECT u.id, u.email, u.role FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, HashKey(tok), time.Now().Unix()).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	return u, err == nil, err
}

func (d *DB) DeleteSession(ctx context.Context, tok string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashKey(tok))
	return err
}

// KeyUser returns the owner of an API key hash and their role, or ok=false.
func (d *DB) KeyUser(ctx context.Context, hash string) (id int64, role rbac.Role, ok bool, err error) {
	err = d.QueryRowContext(ctx, `SELECT u.id, u.role FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.hash = ?`, hash).Scan(&id, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	return id, role, err == nil, err
}

// Spreadsheet is a spreadsheet a user added, with its tabs in the order Google listed them.
type Spreadsheet struct {
	Name, Title string
	GoogleID    string
	Tabs        []string
}

// UserSettings reads userID's spreadsheets and keys into validated Settings.
func (d *DB) UserSettings(ctx context.Context, userID int64) (settings *Settings, err error) {
	err = d.snapshot(ctx, func(tx *sql.Tx) error {
		settings, err = userSettings(ctx, tx, userID)
		return err
	})
	return settings, err
}

// Spreadsheets returns userID's spreadsheets by name, and the Settings they make, read together so
// they agree.
func (d *DB) Spreadsheets(ctx context.Context, userID int64) (sheets []Spreadsheet, settings *Settings, err error) {
	err = d.snapshot(ctx, func(tx *sql.Tx) error {
		err := scan(ctx, tx, `SELECT name, title, google_id FROM spreadsheets WHERE user_id = ? ORDER BY name`, userID, func(rows *sql.Rows) error {
			var s Spreadsheet
			if err := rows.Scan(&s.Name, &s.Title, &s.GoogleID); err != nil {
				return err
			}
			sheets = append(sheets, s)
			return nil
		})
		if err != nil {
			return err
		}
		// rowid keeps tabs in the order Google listed them.
		err = scan(ctx, tx, `SELECT sheet, tab FROM tabs WHERE user_id = ? ORDER BY rowid`, userID, func(rows *sql.Rows) error {
			var sheet, tab string
			if err := rows.Scan(&sheet, &tab); err != nil {
				return err
			}
			i := slices.IndexFunc(sheets, func(s Spreadsheet) bool { return s.Name == sheet })
			sheets[i].Tabs = append(sheets[i].Tabs, tab)
			return nil
		})
		if err != nil {
			return err
		}
		settings, err = userSettings(ctx, tx, userID)
		return err
	})
	return sheets, settings, err
}

// SpreadsheetExists reports whether userID already has a spreadsheet called name.
func (d *DB) SpreadsheetExists(ctx context.Context, userID int64, name string) (bool, error) {
	var taken bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spreadsheets WHERE user_id = ? AND name = ?)`,
		userID, name).Scan(&taken)
	return taken, err
}

// AddSpreadsheet records s and its tabs. Every tab starts off; the user enables ops per tab.
func (d *DB) AddSpreadsheet(ctx context.Context, userID int64, s Spreadsheet) error {
	return d.update(ctx, userID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spreadsheets (user_id, name, google_id, title) VALUES (?, ?, ?, ?)`,
			userID, s.Name, s.GoogleID, s.Title); err != nil {
			return err
		}
		for _, tab := range s.Tabs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tabs (user_id, sheet, tab, ops, hidden) VALUES (?, ?, ?, '[]', '[]')`,
				userID, s.Name, tab); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteSpreadsheet removes userID's spreadsheet called name, with its tabs.
func (d *DB) DeleteSpreadsheet(ctx context.Context, userID int64, name string) error {
	return d.update(ctx, userID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM spreadsheets WHERE user_id = ? AND name = ?`, userID, name)
		return err
	})
}

// SaveTab sets the ops, hidden columns and key column of one of userID's tabs.
func (d *DB) SaveTab(ctx context.Context, userID int64, sheet, tab string, rule TabRule) error {
	return d.update(ctx, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE tabs SET ops = ?, hidden = ?, key_column = ? WHERE user_id = ? AND sheet = ? AND tab = ?`,
			jsonList(rule.Ops), jsonList(rule.Hidden), cmp.Or(rule.Key, "id"), userID, sheet, tab)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, error(ValidationError("unknown tab")))
		}
		return nil
	})
}

// CreateKey stores k (its hash, never the key itself) for userID.
func (d *DB) CreateKey(ctx context.Context, userID int64, k Key) error {
	return d.update(ctx, userID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO api_keys (hash, user_id, name, tabs, ops) VALUES (?, ?, ?, ?, ?)`,
			k.Hash, userID, k.Name, jsonList(k.Tabs), jsonList(k.Ops))
		return err
	})
}

// DeleteKey removes userID's key called name.
func (d *DB) DeleteKey(ctx context.Context, userID int64, name string) error {
	return d.update(ctx, userID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ? AND name = ?`, userID, name)
		return err
	})
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// userSettings reads userID's spreadsheets, tabs and keys from q and validates them.
func userSettings(ctx context.Context, q querier, userID int64) (*Settings, error) {
	settings := &Settings{Spreadsheets: map[string]SheetRule{}}
	err := scan(ctx, q, `SELECT name, google_id FROM spreadsheets WHERE user_id = ?`, userID, func(rows *sql.Rows) error {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return err
		}
		settings.Spreadsheets[name] = SheetRule{ID: id, Tabs: map[string]TabRule{}}
		return nil
	})
	if err == nil {
		err = scan(ctx, q, `SELECT sheet, tab, ops, hidden, key_column FROM tabs WHERE user_id = ?`, userID, func(rows *sql.Rows) error {
			var sheet, tab, ops, hidden string
			var rule TabRule
			if err := rows.Scan(&sheet, &tab, &ops, &hidden, &rule.Key); err != nil {
				return err
			}
			if err := errors.Join(json.Unmarshal([]byte(ops), &rule.Ops), json.Unmarshal([]byte(hidden), &rule.Hidden)); err != nil {
				return err
			}
			settings.Spreadsheets[sheet].Tabs[tab] = rule
			return nil
		})
	}
	if err == nil {
		err = scan(ctx, q, `SELECT hash, name, tabs, ops FROM api_keys WHERE user_id = ? ORDER BY name`, userID, func(rows *sql.Rows) error {
			var k Key
			var tabs, ops string
			if err := rows.Scan(&k.Hash, &k.Name, &tabs, &ops); err != nil {
				return err
			}
			if err := errors.Join(json.Unmarshal([]byte(tabs), &k.Tabs), json.Unmarshal([]byte(ops), &k.Ops)); err != nil {
				return err
			}
			settings.Keys = append(settings.Keys, k)
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	if err := settings.Validate(); err != nil {
		return nil, ValidationError(err.Error())
	}
	return settings, nil
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

// update runs fn in a transaction and commits only if userID's resulting settings still validate,
// so Validate checks every dashboard change.
func (d *DB) update(ctx context.Context, userID int64, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := userSettings(ctx, tx, userID); err != nil {
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
