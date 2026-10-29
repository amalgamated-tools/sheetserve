-- +goose Up
-- Adds the enum type, with its allowed values in options, and a default for new rows. SQLite can't
-- change a CHECK constraint, so the table is rebuilt.
CREATE TABLE columns_new (
	user_id INTEGER NOT NULL,
	sheet TEXT NOT NULL,
	tab TEXT NOT NULL,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK (type IN ('', 'string', 'number', 'integer', 'boolean', 'date', 'datetime', 'enum')),
	required INTEGER NOT NULL DEFAULT 0,
	options TEXT NOT NULL DEFAULT '[]', -- JSON array: an enum's allowed values
	default_value TEXT NOT NULL DEFAULT '', -- the value a create without one gets; '' for none
	PRIMARY KEY (user_id, sheet, tab, name),
	FOREIGN KEY (user_id, sheet, tab) REFERENCES tabs ON DELETE CASCADE
);
INSERT INTO columns_new (user_id, sheet, tab, name, type, required)
	SELECT user_id, sheet, tab, name, type, required FROM columns;
DROP TABLE columns;
ALTER TABLE columns_new RENAME TO columns;

-- +goose Down
DELETE FROM columns WHERE type = 'enum';
CREATE TABLE columns_old (
	user_id INTEGER NOT NULL,
	sheet TEXT NOT NULL,
	tab TEXT NOT NULL,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK (type IN ('', 'string', 'number', 'integer', 'boolean', 'date', 'datetime')),
	required INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, sheet, tab, name),
	FOREIGN KEY (user_id, sheet, tab) REFERENCES tabs ON DELETE CASCADE
);
INSERT INTO columns_old (user_id, sheet, tab, name, type, required)
	SELECT user_id, sheet, tab, name, type, required FROM columns;
DROP TABLE columns;
ALTER TABLE columns_old RENAME TO columns;
