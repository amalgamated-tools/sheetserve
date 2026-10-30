-- +goose Up
-- When Google last refused a user's refresh token, as expired or revoked. A sign-in clears it.
ALTER TABLE users ADD COLUMN google_lost_at INTEGER;

-- +goose Down
ALTER TABLE users DROP COLUMN google_lost_at;
