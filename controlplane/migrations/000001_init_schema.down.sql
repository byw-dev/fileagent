-- Migration: 000001_init_schema (rollback)
-- Description: Drop all objects created by 000001_init_schema.up.sql in
--              reverse dependency order.

-- ---------------------------------------------------------------------------
-- Indexes (dropped automatically with their tables, listed for clarity)
-- ---------------------------------------------------------------------------

DROP INDEX IF EXISTS idx_event_deliveries_retry;
DROP INDEX IF EXISTS idx_upload_logs_status;
DROP INDEX IF EXISTS idx_upload_logs_agent_time;
DROP INDEX IF EXISTS idx_file_entries_path_trgm;
DROP INDEX IF EXISTS idx_file_entries_status;
DROP INDEX IF EXISTS idx_file_entries_bucket;
DROP INDEX IF EXISTS idx_file_entries_agent;
DROP INDEX IF EXISTS idx_file_entries_org_type;
DROP INDEX IF EXISTS idx_agents_last_seen;
DROP INDEX IF EXISTS idx_agents_fingerprint;
DROP INDEX IF EXISTS idx_agents_org_status;

-- ---------------------------------------------------------------------------
-- Tables (reverse dependency order)
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS agent_events;
DROP TABLE IF EXISTS event_deliveries;
DROP TABLE IF EXISTS event_rules;
DROP TABLE IF EXISTS upload_logs;
DROP TABLE IF EXISTS file_entries;
DROP TABLE IF EXISTS file_type_rules;
DROP TABLE IF EXISTS file_types;
DROP TABLE IF EXISTS collection_rules;
DROP TABLE IF EXISTS buckets;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;

-- ---------------------------------------------------------------------------
-- Enum types
-- ---------------------------------------------------------------------------

DROP TYPE IF EXISTS user_role;
DROP TYPE IF EXISTS action_type;
DROP TYPE IF EXISTS event_type;
DROP TYPE IF EXISTS file_status;
DROP TYPE IF EXISTS rule_status;
DROP TYPE IF EXISTS upload_mode;
DROP TYPE IF EXISTS agent_status;

-- ---------------------------------------------------------------------------
-- Extensions (only drop if they were created by this migration)
-- Note: pg_trgm and uuid-ossp may be used by other schemas; use with caution
-- in shared PostgreSQL instances.
-- ---------------------------------------------------------------------------

DROP EXTENSION IF EXISTS "pg_trgm";
DROP EXTENSION IF EXISTS "uuid-ossp";
