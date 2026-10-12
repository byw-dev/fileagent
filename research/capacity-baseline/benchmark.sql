\set ON_ERROR_STOP on
\pset format csv
\pset tuples_only off

CREATE EXTENSION IF NOT EXISTS intarray;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgstattuple;

SELECT 'environment' AS section, version() AS version,
       current_setting('shared_buffers') AS shared_buffers,
       current_setting('work_mem') AS work_mem,
       current_setting('maintenance_work_mem') AS maintenance_work_mem,
       current_setting('effective_cache_size') AS effective_cache_size,
       current_setting('full_page_writes') AS full_page_writes,
       current_setting('wal_compression') AS wal_compression,
       current_setting('checkpoint_timeout') AS checkpoint_timeout,
       current_setting('max_wal_size') AS max_wal_size;

\echo DATASET :scale
SELECT checkpoints_timed AS timed_start,checkpoints_req AS requested_start FROM pg_stat_bgwriter \gset
SELECT wal_fpi AS fpi_start FROM pg_stat_wal \gset
DROP TABLE IF EXISTS fe_eav, ft_eav, fe_arr CASCADE;
CREATE TABLE fe_eav (id uuid NOT NULL, created_at timestamptz NOT NULL,
                     storage_path text NOT NULL, PRIMARY KEY(id), UNIQUE(storage_path));
CREATE TABLE ft_eav (file_entry_id uuid NOT NULL, key text NOT NULL,
                     value text NOT NULL, source text NOT NULL DEFAULT 'path_var',
                     created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(file_entry_id,key));
CREATE INDEX ft_eav_kv ON ft_eav(key,value);
CREATE TABLE fe_arr (id uuid NOT NULL, created_at timestamptz NOT NULL,
                     storage_path text NOT NULL, tag_ids int4[] NOT NULL,
                     PRIMARY KEY(id), UNIQUE(storage_path));
CREATE INDEX fe_arr_tags ON fe_arr USING gin(tag_ids gin__int_ops);

SELECT pg_current_wal_lsn() AS lsn \gset
CHECKPOINT;
SELECT pg_current_wal_lsn() AS bulk_start \gset
SELECT pg_current_wal_lsn() AS eav_files_start \gset
INSERT INTO fe_eav
SELECT md5('20261012:'||g)::uuid,
       timestamptz '2023-01-01 00:00:00+00' + (((g::bigint * 7919) % 94608000)) * interval '1 second',
       'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01 00:00:00+00' + (((g::bigint * 7919) % 94608000)) * interval '1 second','YYYY/MM/DD')||'/'||g||'.dat'
FROM generate_series(1,:scale) g;
SELECT 'bulk_eav_file_rows' AS metric, pg_wal_lsn_diff(pg_current_wal_lsn(), :'eav_files_start') AS wal_bytes,
       :scale AS rows, pg_wal_lsn_diff(pg_current_wal_lsn(), :'eav_files_start')/:scale::numeric AS wal_bytes_per_file;
SELECT pg_current_wal_lsn() AS array_start \gset
INSERT INTO fe_arr
SELECT id,created_at,storage_path,
 ARRAY[1001+(n%1000), CASE WHEN n%10<4 THEN 2001 ELSE 2002+(n%19) END,
       3001+(n%4),4001+(n%3),5001+(n%10),6001+(n%50),7001+(n%20),8001+(n%100)]::int4[]
FROM (SELECT md5('20261012:'||g)::uuid id,
       timestamptz '2023-01-01 00:00:00+00' + (((g::bigint * 7919) % 94608000)) * interval '1 second' created_at,
       'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01 00:00:00+00' + (((g::bigint * 7919) % 94608000)) * interval '1 second','YYYY/MM/DD')||'/'||g||'.dat' storage_path,
       g n FROM generate_series(1,:scale) g) s;
SELECT 'bulk_array_rows' AS metric, pg_wal_lsn_diff(pg_current_wal_lsn(), :'array_start') AS wal_bytes,
       :scale AS rows, pg_wal_lsn_diff(pg_current_wal_lsn(), :'array_start')/:scale::numeric AS wal_bytes_per_file;
SELECT pg_current_wal_lsn() AS tag_rows_start \gset
INSERT INTO ft_eav(file_entry_id,key,value)
SELECT md5('20261012:'||g)::uuid,k.key,k.value
FROM generate_series(1,:scale) g CROSS JOIN LATERAL (VALUES
 ('site','S'||lpad((1+(g%1000))::text,4,'0')),
 ('product',CASE WHEN g%10<4 THEN 'P01' ELSE 'P'||lpad((2+(g%19))::text,2,'0') END),
 ('level','L'||(1+(g%4))),('qc','Q'||(1+(g%3))),
 ('vendor','V'||(1+(g%10))),('model','M'||(1+(g%50))),
 ('region','R'||(1+(g%20))),('instrument','I'||(1+(g%100)))
) k(key,value);
SELECT 'bulk_eav_tag_rows' AS metric, pg_wal_lsn_diff(pg_current_wal_lsn(), :'tag_rows_start') AS wal_bytes,
       :scale*8 AS rows, pg_wal_lsn_diff(pg_current_wal_lsn(), :'tag_rows_start')/(:scale*8)::numeric AS wal_bytes_per_tag;
SELECT 'checkpoint_delta_during_bulk' AS metric,
       checkpoints_timed-:'timed_start'::bigint AS timed_checkpoints,
       checkpoints_req-:'requested_start'::bigint AS requested_checkpoints,
       (SELECT wal_fpi-:'fpi_start'::bigint FROM pg_stat_wal) AS full_page_images,
       current_setting('full_page_writes') AS full_page_writes,
       current_setting('checkpoint_timeout') AS checkpoint_timeout,
       current_setting('max_wal_size') AS max_wal_size
FROM pg_stat_bgwriter;

SELECT 'bulk_wal' AS metric,
       pg_wal_lsn_diff(pg_current_wal_lsn(), :'bulk_start') AS wal_bytes,
       :scale AS rows,
       pg_wal_lsn_diff(pg_current_wal_lsn(), :'bulk_start')/:scale::numeric AS wal_bytes_per_file;
ANALYZE fe_eav; ANALYZE fe_arr; ANALYZE ft_eav;

SELECT 'relation_sizes' AS section, relname, pg_relation_size(oid) AS heap_bytes,
       pg_indexes_size(oid) AS indexes_bytes, pg_total_relation_size(oid) AS total_bytes
FROM pg_class WHERE relname IN ('fe_eav','fe_arr','ft_eav') ORDER BY relname;
SELECT 'index_sizes' AS section, relname,indexrelname,pg_relation_size(indexrelid) AS bytes
FROM pg_stat_user_indexes WHERE relname IN ('fe_eav','fe_arr','ft_eav') ORDER BY relname,indexrelname;

\echo EQUIVALENCE_AND_QUERY_BUFFERS
-- Predicate matrix: H = hot product, L = cold site, A = H AND level, O = H OR qc,
-- N = H AND NOT qc. Full match-set EXCEPT checks and top-50 ordered page checks are asserted.
CREATE TEMP TABLE pred(name text, eav text, arr text, eav_count text, arr_count text);
INSERT INTO pred VALUES
 ('high','key=''product'' AND value=''P01''','tag_ids @> ARRAY[2001]','(key,value) IN (( ''product'',''P01'')) GROUP BY file_entry_id HAVING count(*)=1','tag_ids @> ARRAY[2001]'),
 ('low','key=''site'' AND value=''S0001''','tag_ids @> ARRAY[1001]','(key,value) IN (( ''site'',''S0001'')) GROUP BY file_entry_id HAVING count(*)=1','tag_ids @> ARRAY[1001]'),
 ('and2','(key,value) IN (( ''product'',''P01''),( ''level'',''L1'')) GROUP BY file_entry_id HAVING count(*)=2','tag_ids @> ARRAY[2001,3001]','(key,value) IN (( ''product'',''P01''),( ''level'',''L1'')) GROUP BY file_entry_id HAVING count(*)=2','tag_ids @> ARRAY[2001,3001]'),
 ('or2','(key,value) IN (( ''product'',''P01''),( ''qc'',''Q1'')) GROUP BY file_entry_id HAVING count(*)>=1','tag_ids && ARRAY[2001,4001]','(key,value) IN (( ''product'',''P01''),( ''qc'',''Q1'')) GROUP BY file_entry_id HAVING count(*)>=1','tag_ids && ARRAY[2001,4001]'),
 ('and_not','(key,value) IN (( ''product'',''P01'')) GROUP BY file_entry_id HAVING count(*)=1','tag_ids @> ARRAY[2001] AND NOT (tag_ids @> ARRAY[4001])','(key,value) IN (( ''product'',''P01'')) GROUP BY file_entry_id HAVING count(*)=1','tag_ids @> ARRAY[2001] AND NOT (tag_ids @> ARRAY[4001])');
-- EAV AND-NOT excludes Q1 explicitly in the query below; this keeps today's grouped-positive shape.
DO $$ DECLARE p record; mismatches bigint; BEGIN
 FOR p IN SELECT * FROM pred LOOP
   IF p.name='and_not' THEN
     EXECUTE 'SELECT count(*) FROM ((SELECT file_entry_id id FROM ft_eav WHERE key=''product'' AND value=''P01'') EXCEPT (SELECT file_entry_id FROM ft_eav WHERE key=''qc'' AND value=''Q1'')) e FULL JOIN ((SELECT id FROM fe_arr WHERE tag_ids @> ARRAY[2001] AND NOT(tag_ids @> ARRAY[4001]))) a USING(id) WHERE e.id IS NULL OR a.id IS NULL' INTO mismatches;
   ELSE
     EXECUTE format('SELECT count(*) FROM ((SELECT file_entry_id id FROM ft_eav WHERE %s) EXCEPT (SELECT id FROM fe_arr WHERE %s)) e FULL JOIN ((SELECT id FROM fe_arr WHERE %s) EXCEPT (SELECT file_entry_id FROM ft_eav WHERE %s)) a USING(id)',p.eav_count,p.arr_count,p.arr,p.eav_count) INTO mismatches;
   END IF;
   IF mismatches<>0 THEN RAISE EXCEPTION 'identity mismatch %, rows %',p.name,mismatches; END IF;
 END LOOP;
END $$;
DO $$ DECLARE p record; ec bigint; ac bigint; pm bigint; eq text; aq text; BEGIN
 FOR p IN SELECT * FROM pred LOOP
   IF p.name='and_not' THEN
     eq:='SELECT id FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE key=''product'' AND value=''P01'' GROUP BY file_entry_id HAVING count(*)=1) AND NOT EXISTS (SELECT 1 FROM ft_eav t WHERE t.file_entry_id=fe_eav.id AND t.key=''qc'' AND t.value=''Q1'')';
   ELSE
     eq:=format('SELECT id FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE %s)',p.eav_count);
   END IF;
   aq:=format('SELECT id FROM fe_arr WHERE %s',p.arr);
   EXECUTE 'SELECT count(*) FROM ('||eq||') s' INTO ec;
   EXECUTE 'SELECT count(*) FROM ('||aq||') s' INTO ac;
   IF ec<>ac THEN RAISE EXCEPTION 'count mismatch %, EAV %, array %',p.name,ec,ac; END IF;
   EXECUTE 'WITH e AS ('||eq||' ORDER BY created_at DESC,id DESC LIMIT 50), a AS ('||aq||' ORDER BY created_at DESC,id DESC LIMIT 50) SELECT count(*) FROM ((SELECT id FROM e EXCEPT SELECT id FROM a) UNION ALL (SELECT id FROM a EXCEPT SELECT id FROM e)) x' INTO pm;
   IF pm<>0 THEN RAISE EXCEPTION 'page mismatch %, rows %',p.name,pm; END IF;
   RAISE NOTICE 'equivalence predicate=% matched_rows=% page_rows=50 identical=true',p.name,ec;
 END LOOP;
END $$;

-- Result counts and page-set checks. EXPLAIN JSON captures shared hit+read buffers for both paths.
SELECT format('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE %s) ORDER BY created_at DESC,id DESC LIMIT 50',eav_count) FROM pred WHERE name<>'and_not'
UNION ALL SELECT 'EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE key=''product'' AND value=''P01'' GROUP BY file_entry_id HAVING count(*)=1) AND NOT EXISTS (SELECT 1 FROM ft_eav t WHERE t.file_entry_id=fe_eav.id AND t.key=''qc'' AND t.value=''Q1'') ORDER BY created_at DESC,id DESC LIMIT 50'
\gexec
\echo UNPARTITIONED_TIME_FILTER_QUERY_PLANS
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_arr WHERE created_at >= timestamptz '2025-07-01 00:00:00+00' AND tag_ids @> ARRAY[2001] ORDER BY created_at DESC,id DESC LIMIT 50;
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_arr WHERE created_at >= timestamptz '2025-07-01 00:00:00+00' AND tag_ids @> ARRAY[1001] ORDER BY created_at DESC,id DESC LIMIT 50;
SELECT format('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_arr WHERE %s ORDER BY created_at DESC,id DESC LIMIT 50',arr) FROM pred
\gexec
SELECT format('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT count(*) FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE %s)',eav_count) FROM pred WHERE name<>'and_not'
UNION ALL SELECT 'EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT count(*) FROM fe_eav WHERE id IN (SELECT file_entry_id FROM ft_eav WHERE key=''product'' AND value=''P01'' GROUP BY file_entry_id HAVING count(*)=1) AND NOT EXISTS (SELECT 1 FROM ft_eav t WHERE t.file_entry_id=fe_eav.id AND t.key=''qc'' AND t.value=''Q1'')'
\gexec
SELECT format('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT count(*) FROM fe_arr WHERE %s',arr) FROM pred
\gexec

-- Post-load single-row statements (100 files per model, after an explicit checkpoint).
CHECKPOINT;
SELECT wal_fpi AS single_eav_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS single_eav_start \gset
DO $$ DECLARE i int; fid uuid; BEGIN FOR i IN 1..100 LOOP
 fid:=md5('single-eav:'||i)::uuid;
 INSERT INTO fe_eav VALUES(fid,now(),'agent-single/2026/10/12/'||i||'.dat');
 INSERT INTO ft_eav(file_entry_id,key,value) VALUES
   (fid,'product','P01'),(fid,'site','S001'),(fid,'level','L1'),(fid,'qc','Q1'),
   (fid,'vendor','V1'),(fid,'model','M1'),(fid,'region','R1'),(fid,'instrument','I1');
END LOOP; END $$;
SELECT 'single_eav_wal_per_file' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'single_eav_start')/100::numeric wal_bytes_per_file,
       (SELECT wal_fpi-:'single_eav_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;
CHECKPOINT;
SELECT wal_fpi AS single_arr_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS single_arr_start \gset
DO $$ DECLARE i int; BEGIN FOR i IN 1..100 LOOP
 INSERT INTO fe_arr VALUES(md5('single-arr:'||i)::uuid,now(),'agent-single/2026/10/12/'||i||'.dat',ARRAY[2001,1001,3001,4001,5001,6001,7001,8001]);
END LOOP; END $$;
SELECT 'single_array_wal_per_file' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'single_arr_start')/100::numeric wal_bytes_per_file,
       (SELECT wal_fpi-:'single_arr_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;

-- UUID v4 vs v7 index experiment, random insertion order after a checkpoint.
DROP TABLE IF EXISTS uuid4_idx,uuid7_idx;
CREATE TABLE uuid4_idx(id uuid PRIMARY KEY, payload int NOT NULL);
CREATE TABLE uuid7_idx(id uuid PRIMARY KEY, payload int NOT NULL);
CHECKPOINT;
SELECT wal_fpi AS uuid4_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS uuid4_start \gset
INSERT INTO uuid4_idx SELECT (substr(h,1,8)||'-'||substr(h,9,4)||'-4'||substr(h,14,3)||'-8'||substr(h,18,3)||'-'||substr(h,21,12))::uuid,g
FROM (SELECT g,md5('v4:'||g) h FROM generate_series(1,:scale) g) s ORDER BY md5(g::text);
SELECT 'uuid4_bulk_wal' metric, pg_wal_lsn_diff(pg_current_wal_lsn(),:'uuid4_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'uuid4_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;
CHECKPOINT;
SELECT wal_fpi AS uuid7_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS uuid7_start \gset
INSERT INTO uuid7_idx SELECT (substr(ts,1,8)||'-'||substr(ts,9,4)||'-7'||substr(md5(g::text),6,3)||'-8'||substr(md5(g::text),10,3)||'-'||substr(md5(g::text),13,12))::uuid,g
FROM (SELECT g,lpad(to_hex((extract(epoch FROM timestamptz '2023-01-01 00:00:00+00')*1000)::bigint + g/1000),12,'0') ts FROM generate_series(1,:scale) g) s ORDER BY md5(g::text);
SELECT 'uuid7_bulk_wal' metric, pg_wal_lsn_diff(pg_current_wal_lsn(),:'uuid7_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'uuid7_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;
SELECT 'uuid_index_size' metric, c.relname,pg_relation_size(i.indexrelid) bytes FROM pg_class c JOIN pg_index i ON i.indrelid=c.oid WHERE c.relname IN ('uuid4_idx','uuid7_idx');
SELECT * FROM pgstatindex('uuid4_idx_pkey');
SELECT * FROM pgstatindex('uuid7_idx_pkey');

-- Compact int4-key proxy only. Authoritative file_entries-sized UUID index set
-- comparison is repeated in index-set.sql with production UUID column types.
DROP TABLE IF EXISTS index_current,index_reduced;
CREATE TABLE index_current(id uuid PRIMARY KEY, bucket_id int NOT NULL,storage_path text NOT NULL,org_id int NOT NULL,file_type_id int,agent_id int,status text,uploaded_at timestamptz,UNIQUE(bucket_id,storage_path));
CREATE INDEX idx_current_org_type ON index_current(org_id,file_type_id,uploaded_at DESC);
CREATE INDEX idx_current_agent ON index_current(agent_id,uploaded_at DESC);
CREATE INDEX idx_current_bucket ON index_current(bucket_id,storage_path);
CREATE INDEX idx_current_status ON index_current(status) WHERE status!='completed';
CREATE INDEX idx_current_path_trgm ON index_current USING gin(storage_path gin_trgm_ops);
CREATE TABLE index_reduced (LIKE index_current INCLUDING DEFAULTS);
ALTER TABLE index_reduced ADD PRIMARY KEY(id);
ALTER TABLE index_reduced ADD UNIQUE(bucket_id,storage_path);
CREATE INDEX idx_reduced_org_type ON index_reduced(org_id,file_type_id,uploaded_at DESC);
CREATE INDEX idx_reduced_agent ON index_reduced(agent_id,uploaded_at DESC);
CREATE INDEX idx_reduced_status ON index_reduced(status) WHERE status!='completed';
CHECKPOINT;
SELECT wal_fpi AS idx_current_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS idx_current_start \gset
INSERT INTO index_current SELECT id,g%10000,storage_path,g%100,g%20,g%32,CASE WHEN g%20=0 THEN 'failed' ELSE 'completed' END,created_at FROM (SELECT *,row_number() over() g FROM fe_eav) s;
SELECT 'index_current_wal' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'idx_current_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'idx_current_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;
CHECKPOINT;
SELECT wal_fpi AS idx_reduced_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() AS idx_reduced_start \gset
INSERT INTO index_reduced SELECT id,bucket_id,storage_path,org_id,file_type_id,agent_id,status,uploaded_at FROM index_current;
SELECT 'index_reduced_wal' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'idx_reduced_start')/:scale::numeric wal_bytes_per_row,
       (SELECT wal_fpi-:'idx_reduced_fpi_start'::bigint FROM pg_stat_wal) AS full_page_images;
SELECT 'index_profile' metric,relname,pg_relation_size(oid) heap_bytes,pg_indexes_size(oid) indexes_bytes,pg_total_relation_size(oid) total_bytes FROM pg_class WHERE relname IN('index_current','index_reduced');

-- Partitioning check: 36 monthly partitions, same row identities and tags.
DROP TABLE IF EXISTS fe_part CASCADE;
CREATE TABLE fe_part(id uuid NOT NULL,created_at timestamptz NOT NULL,tag_ids int4[] NOT NULL) PARTITION BY RANGE(created_at);
DO $$ DECLARE d date; BEGIN FOR i IN 0..35 LOOP d:=date '2023-01-01'+(i||' months')::interval; EXECUTE format('CREATE TABLE fe_part_%s PARTITION OF fe_part FOR VALUES FROM (%L) TO (%L)',to_char(d,'YYYYMM'),d,d+interval '1 month'); END LOOP; END $$;
CREATE INDEX fe_part_tags ON fe_part USING gin(tag_ids gin__int_ops);
INSERT INTO fe_part SELECT a.id,a.created_at,a.tag_ids FROM fe_arr a WHERE a.created_at < timestamptz '2026-01-01 00:00:00+00';
ANALYZE fe_part;
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_part WHERE tag_ids @> ARRAY[2001] ORDER BY created_at DESC,id DESC LIMIT 50;
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_part WHERE tag_ids @> ARRAY[1001] ORDER BY created_at DESC,id DESC LIMIT 50;
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_part WHERE created_at >= timestamptz '2025-07-01 00:00:00+00' AND tag_ids @> ARRAY[2001] ORDER BY created_at DESC,id DESC LIMIT 50;
EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM fe_part WHERE created_at >= timestamptz '2025-07-01 00:00:00+00' AND tag_ids @> ARRAY[1001] ORDER BY created_at DESC,id DESC LIMIT 50;
DO $$ DECLARE q record; part record; plan jsonb; stmt text; total_rows int; parts int; hits int; reads int; with_range boolean;
BEGIN
 FOR q IN SELECT * FROM (VALUES ('high','tag_ids @> ARRAY[2001]'),('low','tag_ids @> ARRAY[1001]')) x(name,predicate) LOOP
  FOR with_range IN SELECT false UNION ALL SELECT true LOOP
   total_rows:=0; parts:=0; hits:=0; reads:=0;
   FOR part IN SELECT inhrelid::regclass AS rel FROM pg_inherits WHERE inhparent='fe_part'::regclass ORDER BY inhrelid::regclass::text DESC LOOP
    stmt:=format('EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT id FROM %s WHERE %s %s ORDER BY created_at DESC,id DESC LIMIT %s',part.rel,q.predicate,CASE WHEN with_range THEN 'AND created_at >= timestamptz ''2025-07-01 00:00:00+00''' ELSE '' END,50-total_rows);
    EXECUTE stmt INTO plan;
    plan:=plan->0->'Plan';
    total_rows:=total_rows+coalesce((plan->>'Actual Rows')::int,0);
    hits:=hits+coalesce((plan->>'Shared Hit Blocks')::int,0);
    reads:=reads+coalesce((plan->>'Shared Read Blocks')::int,0);
    parts:=parts+1;
    EXIT WHEN total_rows>=50;
   END LOOP;
   RAISE NOTICE 'chunked_scan predicate=% time_range=% partitions=% returned_rows=% shared_hit_blocks=% shared_read_blocks=%',q.name,with_range,parts,total_rows,hits,reads;
  END LOOP;
 END LOOP;
END $$;
