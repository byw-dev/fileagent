\set ON_ERROR_STOP on
\pset format csv
\pset tuples_only off

SELECT 'environment' AS section, version() AS version,
       current_setting('shared_buffers') AS shared_buffers,
       current_setting('work_mem') AS work_mem,
       current_setting('maintenance_work_mem') AS maintenance_work_mem,
       current_setting('effective_cache_size') AS effective_cache_size,
       current_setting('full_page_writes') AS full_page_writes,
       current_setting('wal_compression') AS wal_compression,
       current_setting('checkpoint_timeout') AS checkpoint_timeout,
       current_setting('max_wal_size') AS max_wal_size;

CREATE EXTENSION IF NOT EXISTS pg_trgm;
DROP TABLE IF EXISTS index_current_uuid,index_reduced_uuid;
CREATE TABLE index_current_uuid(
 id uuid PRIMARY KEY, bucket_id uuid NOT NULL, storage_path text NOT NULL,
 org_id uuid NOT NULL, file_type_id uuid, agent_id uuid, status text,
 uploaded_at timestamptz, UNIQUE(bucket_id,storage_path));
CREATE INDEX idx_current_uuid_org_type ON index_current_uuid(org_id,file_type_id,uploaded_at DESC);
CREATE INDEX idx_current_uuid_agent ON index_current_uuid(agent_id,uploaded_at DESC);
CREATE INDEX idx_current_uuid_bucket ON index_current_uuid(bucket_id,storage_path);
CREATE INDEX idx_current_uuid_status ON index_current_uuid(status) WHERE status!='completed';
CREATE INDEX idx_current_uuid_path_trgm ON index_current_uuid USING gin(storage_path gin_trgm_ops);
CREATE TABLE index_reduced_uuid (LIKE index_current_uuid INCLUDING DEFAULTS);
ALTER TABLE index_reduced_uuid ADD PRIMARY KEY(id);
ALTER TABLE index_reduced_uuid ADD UNIQUE(bucket_id,storage_path);
CREATE INDEX idx_reduced_uuid_org_type ON index_reduced_uuid(org_id,file_type_id,uploaded_at DESC);
CREATE INDEX idx_reduced_uuid_agent ON index_reduced_uuid(agent_id,uploaded_at DESC);
CREATE INDEX idx_reduced_uuid_status ON index_reduced_uuid(status) WHERE status!='completed';

CHECKPOINT;
SELECT wal_fpi AS current_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS current_start \gset
INSERT INTO index_current_uuid
SELECT md5('20261012:'||g)::uuid,
       md5('bucket:'||(g%10000))::uuid,
       'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01 00:00:00+00' + (((g::bigint*7919)%94608000))*interval '1 second','YYYY/MM/DD')||'/'||g||'.dat',
       md5('org:'||(g%100))::uuid,
       CASE WHEN g%11=0 THEN NULL ELSE md5('type:'||(g%20))::uuid END,
       CASE WHEN g%13=0 THEN NULL ELSE md5('agent:'||(g%32))::uuid END,
       CASE WHEN g%20=0 THEN 'failed' ELSE 'completed' END,
       timestamptz '2023-01-01 00:00:00+00' + (((g::bigint*7919)%94608000))*interval '1 second'
FROM generate_series(1,:scale) g;
SELECT 'current_7_indexes' metric,
       pg_wal_lsn_diff(pg_current_wal_lsn(),:'current_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'current_fpi_start'::bigint FROM pg_stat_wal) full_page_images;

CHECKPOINT;
SELECT wal_fpi AS reduced_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS reduced_start \gset
INSERT INTO index_reduced_uuid
SELECT * FROM index_current_uuid;
SELECT 'reduced_5_indexes' metric,
       pg_wal_lsn_diff(pg_current_wal_lsn(),:'reduced_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'reduced_fpi_start'::bigint FROM pg_stat_wal) full_page_images;

SELECT 'index_profile' metric,relname,pg_relation_size(oid) heap_bytes,
       pg_indexes_size(oid) indexes_bytes,pg_total_relation_size(oid) total_bytes
FROM pg_class WHERE relname IN('index_current_uuid','index_reduced_uuid') ORDER BY relname;
SELECT 'index_sizes' metric,relname,indexrelname,pg_relation_size(indexrelid) bytes
FROM pg_stat_user_indexes WHERE relname IN('index_current_uuid','index_reduced_uuid')
ORDER BY relname,indexrelname;
