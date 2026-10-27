-- Migration: 000011_storage_event_source
-- Description: B3-PR3b (DECISIONS.md D-037, design decisions D3+D4):
--   D3: extend the file_entries.source CHECK with 'storage_readback' — the
--       provenance of index facts written by the SeaweedFS filer-subscription
--       adapter after re-reading object state over S3.
--   D4: durable subscription cursor + pending-reconciliation marks. Both tables
--       are consumed by the storage-event-source adapter; the MARKS table is
--       deliberately WRITE-ONLY in this change (its consumer is L2
--       reconciliation, IC-13, out of scope here).
--
-- Append-only per the migrations contract: 000006 added the source column with
-- an inline CHECK, which PostgreSQL named file_entries_source_check (verified
-- live); the up migration replaces it in place and the down migration restores
-- the previous definition verbatim.

ALTER TABLE file_entries DROP CONSTRAINT file_entries_source_check;
ALTER TABLE file_entries ADD CONSTRAINT file_entries_source_check
    CHECK (source IN ('agent', 'api', 'minio_event', 'audit', 'storage_readback'));

-- One row per storage-event-source instance (source_id, e.g.
-- "seaweedfs:<filer addr>"): the last idle-heartbeat declared value the
-- adapter durably claimed (filer clock, ns). Upserts must never move
-- declared_ts_ns backwards (GREATEST guard, see queries/storage_event.sql).
CREATE TABLE storage_event_cursors (
    source_id      TEXT PRIMARY KEY,
    declared_ts_ns BIGINT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE storage_event_cursors IS
    'Durable storage-event-source subscription cursors: per source_id, the last idle-heartbeat declared value (source clock ns) claimed after all earlier events were applied. Written only by the storage event source adapter (B3-PR3b).';

-- Pending-reconciliation marks. WRITE-ONLY for now: the consumer is the L2
-- reconciliation worker (IC-13), which is not part of PR3b — nothing reads
-- these rows yet. resolved_at stays NULL until that consumer exists.
CREATE TABLE storage_event_marks (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_id   TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN ('gap', 'cursor_before_earliest_log', 'bucket_removed')),
    bucket      TEXT,
    from_ts_ns  BIGINT,
    to_ts_ns    BIGINT,
    detail      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);

COMMENT ON TABLE storage_event_marks IS
    'Pending-reconciliation marks for the storage event source (gap windows after reconnects, cursor-before-earliest-log, bucket-removed incidents). WRITE-ONLY in PR3b: the consumer is L2 reconciliation (IC-13, not yet built) — do not build readers against this table before that lands.';
