-- +goose Up
-- Each column of a tab that the user has typed, by its name in the header row. A tab with no rows
-- here is untyped, as tabs were before this table. type '' means the column takes any value.
CREATE TABLE columns (
	user_id INTEGER NOT NULL,
	sheet TEXT NOT NULL,
	tab TEXT NOT NULL,
	name TEXT NOT NULL,
	type TEXT NOT NULL CHECK (type IN ('', 'string', 'number', 'integer', 'boolean', 'date', 'datetime')),
	required INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, sheet, tab, name),
	FOREIGN KEY (user_id, sheet, tab) REFERENCES tabs ON DELETE CASCADE
);

-- +goose Down
DROP TABLE columns;
