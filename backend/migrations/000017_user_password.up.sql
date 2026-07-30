-- 000017: bcrypt password hash for boss/admin login (CONTRACT §19 / BE-A-01).
--
-- users already holds role/phone_hash/status/last_login_at (migration 000001);
-- real login (boss phone+password, admin username+password) needs a stored
-- bcrypt hash. Nullable to stay compatible with existing rows and with the
-- WeChat-only login path; the auth service rejects login when password_hash is
-- empty rather than relying on a NOT NULL constraint.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_hash TEXT;
