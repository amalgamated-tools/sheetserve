-- +goose Up
-- See internal/rbac. New users wait as pending, except the very first, who becomes admin.
-- Users from before roles keep their access as members, and the oldest becomes admin.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'pending' CHECK (role IN ('admin', 'member', 'pending'));
UPDATE users SET role = 'member';
UPDATE users SET role = 'admin' WHERE id = (SELECT min(id) FROM users);

-- +goose Down
ALTER TABLE users DROP COLUMN role;
