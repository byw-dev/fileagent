-- Migration: 000001_init_schema
-- Description: Initial database schema for FileAgent control plane.
-- Source: docs/design/system-design.md §§3.3, 3.4

-- ---------------------------------------------------------------------------
-- 3.3.1  Extensions & enum types
-- ---------------------------------------------------------------------------

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm"; -- used for file path fuzzy search

-- 7 enum types
CREATE TYPE agent_status AS ENUM ('pending','approved','online','offline','revoked');
CREATE TYPE upload_mode   AS ENUM ('watch','scheduled');
CREATE TYPE rule_status   AS ENUM ('active','inactive');
CREATE TYPE file_status   AS ENUM ('uploading','completed','failed','deleted');
CREATE TYPE event_type    AS ENUM ('file_uploaded','file_deleted','agent_online','agent_offline','agent_approved','agent_revoked');
CREATE TYPE action_type   AS ENUM ('webhook','nats_publish','kafka_publish');
CREATE TYPE user_role     AS ENUM ('super_admin','org_admin','org_viewer');

-- ---------------------------------------------------------------------------
-- 3.3.2  Organizations (multi-tenant placeholder)
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        VARCHAR(128) NOT NULL UNIQUE,
    description TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Default organization for v1 single-org deployment
INSERT INTO organizations (id, name) VALUES
    ('00000000-0000-0000-0000-000000000001', 'default');

-- ---------------------------------------------------------------------------
-- 3.3.3  Users
-- ---------------------------------------------------------------------------

CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    username        VARCHAR(64) NOT NULL,
    email           VARCHAR(256),
    password_hash   VARCHAR(256) NOT NULL,      -- bcrypt
    role            user_role NOT NULL DEFAULT 'org_viewer',
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, username)
);

-- ---------------------------------------------------------------------------
-- 3.3.4  Agents (collectors)
-- ---------------------------------------------------------------------------

CREATE TABLE agents (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id              UUID NOT NULL REFERENCES organizations(id),
    name                VARCHAR(128) NOT NULL,
    fingerprint         VARCHAR(256) NOT NULL UNIQUE, -- device unique identifier
    status              agent_status NOT NULL DEFAULT 'pending',
    auth_token_hash     VARCHAR(256),                -- bcrypt(JWT jti)
    token_expires_at    TIMESTAMPTZ,
    os_info             JSONB NOT NULL DEFAULT '{}', -- OS type, version, hostname
    ip_address          INET,                        -- last connection IP
    approved_by         UUID REFERENCES users(id),
    approved_at         TIMESTAMPTZ,
    revoked_by          UUID REFERENCES users(id),
    revoked_at          TIMESTAMPTZ,
    last_seen_at        TIMESTAMPTZ,
    metadata            JSONB NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.3.5  MinIO bucket registry
-- ---------------------------------------------------------------------------

CREATE TABLE buckets (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    name            VARCHAR(128) NOT NULL UNIQUE, -- MinIO bucket name
    description     TEXT,
    policy_json     JSONB,                        -- IAM policy snapshot
    sts_role_arn    VARCHAR(256),                 -- STS AssumeRole ARN
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.3.6  Collection rules
-- ---------------------------------------------------------------------------

CREATE TABLE collection_rules (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id                UUID NOT NULL REFERENCES organizations(id),
    agent_id              UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    bucket_id             UUID NOT NULL REFERENCES buckets(id),
    name                  VARCHAR(128) NOT NULL,
    mode                  upload_mode NOT NULL,
    status                rule_status NOT NULL DEFAULT 'active',
    source_path_template  TEXT NOT NULL,
    file_glob             VARCHAR(256) NOT NULL DEFAULT '*',
    upload_path_template  TEXT NOT NULL,
    watch_recursive       BOOLEAN NOT NULL DEFAULT FALSE,
    watch_subdir_pattern  VARCHAR(256),
    cron_expr             VARCHAR(64),
    run_once_on_start     BOOLEAN NOT NULL DEFAULT FALSE,
    append_mode           VARCHAR(32) NOT NULL DEFAULT 'overwrite',
    metadata              JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.3.7  File types (logical categorisation)
-- ---------------------------------------------------------------------------

CREATE TABLE file_types (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id      UUID NOT NULL REFERENCES organizations(id),
    name        VARCHAR(128) NOT NULL,
    description TEXT,
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, name)
);

CREATE TABLE file_type_rules (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    file_type_id    UUID NOT NULL REFERENCES file_types(id) ON DELETE CASCADE,
    path_pattern    TEXT NOT NULL,
    priority        INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.3.8  File entries index
-- ---------------------------------------------------------------------------

CREATE TABLE file_entries (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    file_type_id    UUID REFERENCES file_types(id),
    agent_id        UUID REFERENCES agents(id),
    rule_id         UUID REFERENCES collection_rules(id),
    bucket_id       UUID NOT NULL REFERENCES buckets(id),
    storage_path    TEXT NOT NULL,
    original_path   TEXT,
    file_name       VARCHAR(512) NOT NULL,
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    sha256          VARCHAR(64),
    etag            VARCHAR(128),
    content_type    VARCHAR(128),
    file_mtime      TIMESTAMPTZ,
    status          file_status NOT NULL DEFAULT 'uploading',
    uploaded_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (bucket_id, storage_path)
);

-- ---------------------------------------------------------------------------
-- 3.3.9  Upload logs
-- ---------------------------------------------------------------------------

CREATE TABLE upload_logs (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id              UUID NOT NULL REFERENCES organizations(id),
    agent_id            UUID NOT NULL REFERENCES agents(id),
    file_entry_id       UUID REFERENCES file_entries(id),
    rule_id             UUID REFERENCES collection_rules(id),
    original_path       TEXT NOT NULL,
    storage_path        TEXT NOT NULL,
    size_bytes          BIGINT NOT NULL DEFAULT 0,
    bytes_transferred   BIGINT NOT NULL DEFAULT 0,
    status              VARCHAR(32) NOT NULL,
    error_message       TEXT,
    retry_count         INT NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL,
    finished_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.3.10  Event rules & deliveries
-- ---------------------------------------------------------------------------

CREATE TABLE event_rules (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    name            VARCHAR(128) NOT NULL,
    event_type      event_type NOT NULL,
    filter          JSONB NOT NULL DEFAULT '{}',
    action_type     action_type NOT NULL,
    action_config   JSONB NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_by      UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE event_deliveries (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    event_rule_id   UUID NOT NULL REFERENCES event_rules(id),
    event_type      event_type NOT NULL,
    payload         JSONB NOT NULL,
    status          VARCHAR(32) NOT NULL,
    response_code   INT,
    response_body   TEXT,
    attempt_count   INT NOT NULL DEFAULT 0,
    next_retry_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);

-- ---------------------------------------------------------------------------
-- 3.3.11  Agent online/offline event log
-- ---------------------------------------------------------------------------

CREATE TABLE agent_events (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id    UUID NOT NULL REFERENCES agents(id),
    event_type  VARCHAR(32) NOT NULL,
    ip_address  INET,
    detail      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3.4  Index strategy
-- ---------------------------------------------------------------------------

-- Agent queries
CREATE INDEX idx_agents_org_status  ON agents (org_id, status);
CREATE INDEX idx_agents_fingerprint ON agents (fingerprint);
CREATE INDEX idx_agents_last_seen   ON agents (last_seen_at DESC);

-- File entry queries (high-frequency)
CREATE INDEX idx_file_entries_org_type  ON file_entries (org_id, file_type_id, uploaded_at DESC);
CREATE INDEX idx_file_entries_agent     ON file_entries (agent_id, uploaded_at DESC);
CREATE INDEX idx_file_entries_bucket    ON file_entries (bucket_id, storage_path);
CREATE INDEX idx_file_entries_status    ON file_entries (status) WHERE status != 'completed';

-- Path fuzzy search
CREATE INDEX idx_file_entries_path_trgm ON file_entries USING gin (storage_path gin_trgm_ops);

-- Upload logs
CREATE INDEX idx_upload_logs_agent_time ON upload_logs (agent_id, created_at DESC);
CREATE INDEX idx_upload_logs_status     ON upload_logs (status, created_at DESC);

-- Event delivery retries
CREATE INDEX idx_event_deliveries_retry ON event_deliveries (next_retry_at) WHERE status = 'failed';
