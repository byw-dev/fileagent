-- Migration: 000002_seed_default_buckets
-- Description: Seed default bucket records for the default organisation.
--
-- Ensures the database has rows matching the physical MinIO buckets created by
-- deploy/scripts/init-minio.sh. Using ON CONFLICT DO NOTHING makes this
-- migration idempotent so it is safe to apply on existing databases.

INSERT INTO buckets (org_id, name, description)
VALUES
    ('00000000-0000-0000-0000-000000000001', 'data-sensor', 'Default sensor data bucket'),
    ('00000000-0000-0000-0000-000000000001', 'tmp-uploads',  'Temporary uploads staging bucket')
ON CONFLICT (name) DO NOTHING;
