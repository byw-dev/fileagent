BEGIN;

-- ---------------------------------------------------------------------------
-- Metadata / controlled tags (metadata model 6c, Phase 1 — D-025)
-- Design: docs/design/metadata-model.md §P1.1
-- Append-only migration; no existing table is altered. Rule declarations live
-- in the existing collection_rules.metadata JSONB (no rules table change).
-- ---------------------------------------------------------------------------

-- Tag key vocabulary (key strictly controlled).
CREATE TABLE tag_keys (
    id                     UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id                 UUID NOT NULL REFERENCES organizations(id),
    key                    VARCHAR(64) NOT NULL,           -- vendor / model / site / ...
    label                  VARCHAR(128) NOT NULL,
    value_controlled       BOOLEAN NOT NULL DEFAULT TRUE,  -- values must exist in tag_values
    required_at_collection BOOLEAN NOT NULL DEFAULT FALSE,
    allow_path_var         BOOLEAN NOT NULL DEFAULT TRUE,  -- may be mapped from a path variable
    system_reserved        BOOLEAN NOT NULL DEFAULT FALSE, -- e.g. level; cannot be deleted
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key)
);

-- Legal values for each controlled key (value controlled but extensible).
CREATE TABLE tag_values (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tag_key_id  UUID NOT NULL REFERENCES tag_keys(id) ON DELETE CASCADE,
    value       VARCHAR(128) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tag_key_id, value)
);

-- File <-> tag (at most one value per file x key). key is denormalised for
-- straightforward faceted queries.
CREATE TABLE file_tags (
    file_entry_id UUID NOT NULL REFERENCES file_entries(id) ON DELETE CASCADE,
    key           VARCHAR(64) NOT NULL,
    value         VARCHAR(128) NOT NULL,
    source        VARCHAR(16) NOT NULL,                    -- rule_static | path_var | manual | api
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_entry_id, key)
);
CREATE INDEX idx_file_tags_kv ON file_tags (key, value);

-- Pending values queue: path-variable values not yet in the vocabulary.
CREATE TABLE pending_tag_values (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    tag_key_id      UUID NOT NULL REFERENCES tag_keys(id) ON DELETE CASCADE,
    extracted_value VARCHAR(128) NOT NULL,
    source          VARCHAR(24) NOT NULL,                  -- path_var | path_backfill
    source_rule_id  UUID REFERENCES collection_rules(id),
    hit_count       INT NOT NULL DEFAULT 1,
    suggested_value VARCHAR(128),                          -- likely match against an existing value
    status          VARCHAR(16) NOT NULL DEFAULT 'pending',
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tag_key_id, extracted_value)
);

-- Retag audit (manual / api / retro tasks).
CREATE TABLE tag_audit (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id        UUID NOT NULL REFERENCES organizations(id),
    file_entry_id UUID REFERENCES file_entries(id) ON DELETE SET NULL,
    key           VARCHAR(64) NOT NULL,
    old_value     VARCHAR(128),
    new_value     VARCHAR(128),
    action        VARCHAR(24) NOT NULL,                    -- set | merge | retag | clear
    actor_user_id UUID REFERENCES users(id),
    source        VARCHAR(16) NOT NULL,                    -- manual | api | retro
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_tag_audit_file ON tag_audit (file_entry_id);

COMMIT;
