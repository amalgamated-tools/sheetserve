-- IF NOT EXISTS lets databases made before goose, which already have these tables, adopt it.

-- +goose Up
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
);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE tabs;
DROP TABLE spreadsheets;
DROP TABLE sessions;
DROP TABLE users;
