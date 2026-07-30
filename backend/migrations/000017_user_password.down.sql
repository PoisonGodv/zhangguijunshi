-- Reverse 000017: drop the login password column.

ALTER TABLE users
    DROP COLUMN IF EXISTS password_hash;
