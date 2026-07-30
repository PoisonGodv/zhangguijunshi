-- Reverse 000018: drop the username unique index and the version/username columns.

DROP INDEX IF EXISTS idx_users_tenant_username;

ALTER TABLE users
    DROP COLUMN IF EXISTS username;

ALTER TABLE users
    DROP COLUMN IF EXISTS version;
