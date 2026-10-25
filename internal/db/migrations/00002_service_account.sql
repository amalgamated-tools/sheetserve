-- +goose Up
-- 1: the spreadsheet is reached with the server's service account, not the owner's Google token.
ALTER TABLE spreadsheets ADD COLUMN service_account INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE spreadsheets DROP COLUMN service_account;
