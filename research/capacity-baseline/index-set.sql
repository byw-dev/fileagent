\set ON_ERROR_STOP on
\pset format csv
\pset tuples_only off

SELECT 'environment' section,version(),current_setting('shared_buffers') shared_buffers,current_setting('work_mem') work_mem,current_setting('maintenance_work_mem') maintenance_work_mem,current_setting('effective_cache_size') effective_cache_size,current_setting('full_page_writes') full_page_writes,current_setting('wal_compression') wal_compression,current_setting('checkpoint_timeout') checkpoint_timeout,current_setting('max_wal_size') max_wal_size;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS pgstattuple;
DROP TABLE IF EXISTS index_v4_uuid,index_v7_ordered_uuid,index_v7_random_uuid;
CREATE TABLE index_v4_uuid(id uuid PRIMARY KEY,bucket_id uuid NOT NULL,storage_path text NOT NULL,org_id uuid NOT NULL,file_type_id uuid,agent_id uuid,status text,uploaded_at timestamptz,UNIQUE(bucket_id,storage_path));
CREATE INDEX idx_v4_uuid_org_type ON index_v4_uuid(org_id,file_type_id,uploaded_at DESC);
CREATE INDEX idx_v4_uuid_agent ON index_v4_uuid(agent_id,uploaded_at DESC);
CREATE INDEX idx_v4_uuid_bucket ON index_v4_uuid(bucket_id,storage_path);
CREATE INDEX idx_v4_uuid_status ON index_v4_uuid(status) WHERE status!='completed';
CREATE INDEX idx_v4_uuid_path_trgm ON index_v4_uuid USING gin(storage_path gin_trgm_ops);
CREATE TABLE index_v7_ordered_uuid (LIKE index_v4_uuid INCLUDING ALL);
CREATE TABLE index_v7_random_uuid (LIKE index_v4_uuid INCLUDING ALL);

-- Each variant uses the same g sequence and non-PK values. Only UUID construction
-- and, for the extra random-v7 control, arrival order differ.
CHECKPOINT;
SELECT wal_fpi v4_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v4_start \gset
INSERT INTO index_v4_uuid SELECT (substr(h,1,8)||'-'||substr(h,9,4)||'-4'||substr(h,14,3)||'-8'||substr(h,18,3)||'-'||substr(h,21,12))::uuid,md5('bucket:'||(g%10000))::uuid,'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second','YYYY/MM/DD')||'/'||g||'.dat',md5('org:'||(g%100))::uuid,CASE WHEN g%11=0 THEN NULL ELSE md5('type:'||(g%20))::uuid END,CASE WHEN g%13=0 THEN NULL ELSE md5('agent:'||(g%32))::uuid END,CASE WHEN g%20=0 THEN 'failed' ELSE 'completed' END,timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second' FROM (SELECT g,md5('v4:'||g) h FROM generate_series(1,:scale) g) s ORDER BY g;
SELECT 'current_7_v4' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v4_start')/:scale::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v4_fpi_start'::bigint FROM pg_stat_wal) full_page_images;
CHECKPOINT;
SELECT wal_fpi v7o_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v7o_start \gset
INSERT INTO index_v7_ordered_uuid SELECT (substr(ts,1,8)||'-'||substr(ts,9,4)||'-7'||substr(md5(g::text),6,3)||'-8'||substr(md5(g::text),10,3)||'-'||substr(md5(g::text),13,12))::uuid,md5('bucket:'||(g%10000))::uuid,'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second','YYYY/MM/DD')||'/'||g||'.dat',md5('org:'||(g%100))::uuid,CASE WHEN g%11=0 THEN NULL ELSE md5('type:'||(g%20))::uuid END,CASE WHEN g%13=0 THEN NULL ELSE md5('agent:'||(g%32))::uuid END,CASE WHEN g%20=0 THEN 'failed' ELSE 'completed' END,timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second' FROM (SELECT g,lpad(to_hex((extract(epoch FROM timestamptz '2023-01-01')*1000)::bigint+g),12,'0') ts FROM generate_series(1,:scale) g) s ORDER BY g;
SELECT 'current_7_v7_ordered' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v7o_start')/:scale::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v7o_fpi_start'::bigint FROM pg_stat_wal) full_page_images;
CHECKPOINT;
SELECT wal_fpi v7r_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v7r_start \gset
INSERT INTO index_v7_random_uuid SELECT (substr(ts,1,8)||'-'||substr(ts,9,4)||'-7'||substr(md5(g::text),6,3)||'-8'||substr(md5(g::text),10,3)||'-'||substr(md5(g::text),13,12))::uuid,md5('bucket:'||(g%10000))::uuid,'agent-'||(g%32)||'/'||to_char(timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second','YYYY/MM/DD')||'/'||g||'.dat',md5('org:'||(g%100))::uuid,CASE WHEN g%11=0 THEN NULL ELSE md5('type:'||(g%20))::uuid END,CASE WHEN g%13=0 THEN NULL ELSE md5('agent:'||(g%32))::uuid END,CASE WHEN g%20=0 THEN 'failed' ELSE 'completed' END,timestamptz '2023-01-01'+(((g::bigint*7919)%94608000))*interval '1 second' FROM (SELECT g,lpad(to_hex((extract(epoch FROM timestamptz '2023-01-01')*1000)::bigint+g),12,'0') ts FROM generate_series(1,:scale) g) s ORDER BY md5(g::text);
SELECT 'current_7_v7_random' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v7r_start')/:scale::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v7r_fpi_start'::bigint FROM pg_stat_wal) full_page_images;

SELECT 'index_profile' metric,relname,pg_relation_size(oid) heap_bytes,pg_indexes_size(oid) indexes_bytes,pg_total_relation_size(oid) total_bytes FROM pg_class WHERE relname IN('index_v4_uuid','index_v7_ordered_uuid','index_v7_random_uuid') ORDER BY relname;
SELECT 'pk_profile' metric,c.relname,pg_relation_size(i.indexrelid) pk_bytes,s.avg_leaf_density,s.leaf_fragmentation FROM pg_class c JOIN pg_index i ON i.indrelid=c.oid AND i.indisprimary CROSS JOIN LATERAL pgstatindex(i.indexrelid::regclass) s WHERE c.relname IN('index_v4_uuid','index_v7_ordered_uuid','index_v7_random_uuid') ORDER BY c.relname;
SELECT 'index_sizes' metric,relname,indexrelname,pg_relation_size(indexrelid) bytes FROM pg_stat_user_indexes WHERE relname IN('index_v4_uuid','index_v7_ordered_uuid','index_v7_random_uuid') ORDER BY relname,indexrelname;

-- One INSERT statement per row, matching the 100-row post-load method in benchmark.sql.
SELECT set_config('capacity.scale',:scale::text,false);
CHECKPOINT;
SELECT wal_fpi v4s_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v4s_start \gset
DO $$ DECLARE i int; h text; BEGIN FOR i IN 1..100 LOOP h:=md5('index-v4-single:'||i); INSERT INTO index_v4_uuid VALUES ((substr(h,1,8)||'-'||substr(h,9,4)||'-4'||substr(h,14,3)||'-8'||substr(h,18,3)||'-'||substr(h,21,12))::uuid,md5('single-bucket')::uuid,'single/v4/'||i,md5('single-org')::uuid,NULL,NULL,'completed',now()); END LOOP; END $$;
SELECT 'current_7_v4_single_wal' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v4s_start')/100::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v4s_fpi_start'::bigint FROM pg_stat_wal) full_page_images;
CHECKPOINT;
SELECT wal_fpi v7os_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v7os_start \gset
DO $$ DECLARE i int; g bigint; ts text; n bigint:=current_setting('capacity.scale')::bigint; BEGIN FOR i IN 1..100 LOOP g:=n+i; ts:=lpad(to_hex((extract(epoch FROM timestamptz '2023-01-01')*1000)::bigint+g),12,'0'); INSERT INTO index_v7_ordered_uuid VALUES ((substr(ts,1,8)||'-'||substr(ts,9,4)||'-7'||substr(md5(g::text),6,3)||'-8'||substr(md5(g::text),10,3)||'-'||substr(md5(g::text),13,12))::uuid,md5('single-bucket')::uuid,'single/v7-ordered/'||i,md5('single-org')::uuid,NULL,NULL,'completed',now()); END LOOP; END $$;
SELECT 'current_7_v7_ordered_single_wal' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v7os_start')/100::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v7os_fpi_start'::bigint FROM pg_stat_wal) full_page_images;
CHECKPOINT;
SELECT wal_fpi v7rs_fpi_start FROM pg_stat_wal \gset
SELECT pg_current_wal_lsn() v7rs_start \gset
DO $$ DECLARE i int; g bigint; ts text; n bigint:=current_setting('capacity.scale')::bigint; BEGIN FOR i IN 1..100 LOOP g:=((i::bigint*7919)%n)+100000000; ts:=lpad(to_hex((extract(epoch FROM timestamptz '2023-01-01')*1000)::bigint+(g%n)),12,'0'); INSERT INTO index_v7_random_uuid VALUES ((substr(ts,1,8)||'-'||substr(ts,9,4)||'-7'||substr(md5(g::text),6,3)||'-8'||substr(md5(g::text),10,3)||'-'||substr(md5(g::text),13,12))::uuid,md5('single-bucket')::uuid,'single/v7-random/'||i,md5('single-org')::uuid,NULL,NULL,'completed',now()); END LOOP; END $$;
SELECT 'current_7_v7_random_single_wal' metric,pg_wal_lsn_diff(pg_current_wal_lsn(),:'v7rs_start')/100::numeric wal_bytes_per_row,(SELECT wal_fpi-:'v7rs_fpi_start'::bigint FROM pg_stat_wal) full_page_images;
