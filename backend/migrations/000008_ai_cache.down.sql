-- ============================================================
-- 000008_ai_cache.down.sql
-- 逆序回滚 000008_ai_cache.up.sql (M3-S2 AI-06)
-- 依据: CONTRACT §13.2 (down 不 drop extension, 保守起见)
--
-- 注意: 仅 DROP TABLE ai_cache, 不 DROP EXTENSION vector。
--   pgvector 扩展可能被其他依赖共享, 且 CREATE EXTENSION IF NOT
--   EXISTS 是幂等的; 保守起见 down 只删表, extension 由 DBA 统一管理。
-- ============================================================

DROP TABLE IF EXISTS ai_cache;
