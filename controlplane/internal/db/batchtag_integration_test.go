//go:build integration

// Integration tests for the hand-written batch tagging mutations. They require a
// running PostgreSQL with migrations applied (see db_integration_test.go).
//
//	cd controlplane && go test ./internal/db/... -tags=integration -run TestBatchTag
package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBatchTagQueries_Integration seeds three files (two carrying site:tokyo, one
// site:osaka) and exercises BatchSetFileTag/BatchClearFileTag against a real DB,
// asserting predicate scoping, per-file audit, and idempotency.
func TestBatchTagQueries_Integration(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("postgres", testDSN(t))
	require.NoError(t, err)
	defer raw.Close()
	require.NoError(t, raw.Ping())

	q := New(raw)
	org := uuid.New()
	_, err = raw.ExecContext(ctx, `INSERT INTO organizations (id, name) VALUES ($1, $2)`, org, "batch-it-"+org.String()[:8])
	require.NoError(t, err)
	var bucket uuid.UUID
	require.NoError(t, raw.QueryRowContext(ctx,
		`INSERT INTO buckets (org_id, name) VALUES ($1, 'b') RETURNING id`, org).Scan(&bucket))

	var siteKey uuid.UUID
	require.NoError(t, raw.QueryRowContext(ctx,
		`INSERT INTO tag_keys (org_id, key, label, value_controlled) VALUES ($1,'site','Site',true) RETURNING id`, org).Scan(&siteKey))

	// Three files: f1/f2 tagged site:tokyo, f3 site:osaka.
	mkFile := func(path, site string) uuid.UUID {
		var id uuid.UUID
		require.NoError(t, raw.QueryRowContext(ctx,
			`INSERT INTO file_entries (org_id, bucket_id, storage_path, file_name, status)
			 VALUES ($1,$2,$3,$4,'completed') RETURNING id`, org, bucket, path, path).Scan(&id))
		_, err := raw.ExecContext(ctx,
			`INSERT INTO file_tags (file_entry_id, key, value, source) VALUES ($1,'site',$2,'path_var')`, id, site)
		require.NoError(t, err)
		return id
	}
	mkFile("a/1", "tokyo")
	mkFile("a/2", "tokyo")
	mkFile("a/3", "osaka")

	tokyoFilter := BatchTagFilter{OrgID: org, Tags: []FileTagFilter{{Key: "site", Value: "tokyo"}}}

	// Batch set vendor=omron on site:tokyo files → f1, f2 only.
	n, err := q.BatchSetFileTag(ctx, BatchSetFileTagParams{
		Filter: tokyoFilter, Key: "vendor", Value: "omron",
		Source: "manual", Action: "set", AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	countVendor := func(val string) int {
		var c int
		require.NoError(t, raw.QueryRowContext(ctx,
			`SELECT count(*) FROM file_tags ft JOIN file_entries fe ON fe.id = ft.file_entry_id
			 WHERE fe.org_id=$1 AND ft.key='vendor' AND ft.value=$2`, org, val).Scan(&c))
		return c
	}
	assert.Equal(t, 2, countVendor("omron")) // f1, f2 tagged; f3 (osaka) not

	var auditSet int
	require.NoError(t, raw.QueryRowContext(ctx,
		`SELECT count(*) FROM tag_audit WHERE org_id=$1 AND key='vendor' AND action='set'`, org).Scan(&auditSet))
	assert.Equal(t, 2, auditSet)

	// Idempotent: re-running the same set changes nothing and audits nothing.
	n, err = q.BatchSetFileTag(ctx, BatchSetFileTagParams{
		Filter: tokyoFilter, Key: "vendor", Value: "omron",
		Source: "manual", Action: "set", AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Batch clear vendor on site:tokyo files → removes from f1, f2.
	n, err = q.BatchClearFileTag(ctx, BatchClearFileTagParams{
		Filter: tokyoFilter, Key: "vendor", Action: "clear", AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	assert.Equal(t, 0, countVendor("omron"))

	// Clear again → nothing left.
	n, err = q.BatchClearFileTag(ctx, BatchClearFileTagParams{
		Filter: tokyoFilter, Key: "vendor", Action: "clear", AuditSource: "manual",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// Cleanup (cascades to file_tags/tag_values; tag_audit file_entry_id → SET NULL).
	_, err = raw.ExecContext(ctx, `DELETE FROM tag_audit WHERE org_id=$1`, org)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `DELETE FROM file_entries WHERE org_id=$1`, org)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `DELETE FROM tag_keys WHERE org_id=$1`, org)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `DELETE FROM buckets WHERE org_id=$1`, org)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `DELETE FROM organizations WHERE id=$1`, org)
	require.NoError(t, err)
}
