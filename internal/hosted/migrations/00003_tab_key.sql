-- +goose Up
-- The column whose value identifies a row. A tab whose header lacks it can only be listed.
ALTER TABLE tabs ADD COLUMN key_column TEXT NOT NULL DEFAULT 'id';

-- +goose Down
ALTER TABLE tabs DROP COLUMN key_column;
