-- Local / CI database bootstrap.
-- Mounted into the postgres container at /docker-entrypoint-initdb.d (read-only)
-- and executed once on first database boot. Safe to re-run (IF NOT EXISTS).

-- pgvector: vector similarity search for embeddings (AI engine, ARCH-01).
CREATE EXTENSION IF NOT EXISTS vector;
