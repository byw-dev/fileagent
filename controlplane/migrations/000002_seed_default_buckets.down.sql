-- Migration: 000002_seed_default_buckets (rollback)
-- Description: Remove the default bucket records seeded by the up migration.
--
-- Only deletes buckets that were created by the seed and have no dependent
-- records (collection_rules, files, etc.).  If dependent rows exist the
-- DELETE is still attempted; the database FK constraints will reject it,
-- which prevents accidental data loss in production.
--
-- The org UUID '00000000-0000-0000-0000-000000000001' matches DefaultOrgID
-- defined in controlplane/internal/bootstrap/admin.go.

DELETE FROM buckets
WHERE org_id = '00000000-0000-0000-0000-000000000001'
  AND name IN ('data-sensor', 'tmp-uploads');
