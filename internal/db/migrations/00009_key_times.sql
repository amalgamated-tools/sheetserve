-- +goose Up
-- When each key was made and last used, as Unix seconds. Keys from before this are NULL.
ALTER TABLE api_keys ADD COLUMN created_at INTEGER;
ALTER TABLE api_keys ADD COLUMN last_used_at INTEGER;

-- +goose Down
ALTER TABLE api_keys DROP COLUMN last_used_at;
ALTER TABLE api_keys DROP COLUMN created_at;
