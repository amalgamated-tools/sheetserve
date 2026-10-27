-- +goose Up
-- The server no longer has a service account. Spreadsheets added through it stay, and are now
-- reached with the owner's Google token, so they fail until the owner picks them in Drive or removes them.
ALTER TABLE spreadsheets DROP COLUMN service_account;

-- +goose Down
ALTER TABLE spreadsheets ADD COLUMN service_account INTEGER NOT NULL DEFAULT 0;
