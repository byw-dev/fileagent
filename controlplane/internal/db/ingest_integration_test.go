//go:build integration

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// guardMutation applies mutations only to generated SQL at the test DB boundary.
// Production SQL and the production fallback are exercised unchanged at baseline.
type guardMutation struct {
	DBTX
	name string
}

// QueryRowContext disables one guard branch or fallback for mutation analysis.
func (m guardMutation) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	if strings.HasPrefix(query, "-- name: UpsertIndexedFile") {
		switch m.name {
		case "no_guard":
			start, end := strings.Index(query, "WHERE EXCLUDED.observed_at"), strings.Index(query, "RETURNING")
			query = query[:start] + query[end:]
		case "no_greater":
			query = strings.Replace(query, "EXCLUDED.observed_at > fe.observed_at", "FALSE", 1)
		case "no_equal":
			query = strings.Replace(query, "EXCLUDED.observed_at = fe.observed_at", "FALSE", 1)
		case "strict_null":
			query = strings.Replace(query, "EXCLUDED.event_seq IS NULL OR fe.event_seq IS NULL\n          OR ", "", 1)
		case "no_lpad":
			query = strings.ReplaceAll(query, "lpad(EXCLUDED.event_seq,32,'0')", "EXCLUDED.event_seq")
			query = strings.ReplaceAll(query, "lpad(fe.event_seq,32,'0')", "fe.event_seq")
		}
	}
	if strings.HasPrefix(query, "-- name: DeleteIndexedFile") {
		switch m.name {
		case "no_guard":
			start, end := strings.Index(query, " AND ($3"), strings.Index(query, "RETURNING")
			query = query[:start] + "\n" + query[end:]
		case "no_greater":
			query = strings.Replace(query, "$3::timestamptz > fe.observed_at", "FALSE", 1)
		case "no_equal":
			query = strings.Replace(query, "$3::timestamptz = fe.observed_at", "FALSE", 1)
		case "no_delete_advance":
			query = strings.Replace(query, "observed_at = $3, event_seq = $4", "observed_at = fe.observed_at, event_seq = fe.event_seq", 1)
		case "no_lpad":
			query = strings.ReplaceAll(query, "lpad($4::text,32,'0')", "$4::text")
			query = strings.ReplaceAll(query, "lpad(fe.event_seq,32,'0')", "fe.event_seq")
		}
	}
	if m.name == "no_fallback" && strings.HasPrefix(query, "-- name: GetIndexedFileByKey") {
		query = strings.TrimSpace(query) + " AND FALSE"
	}
	return m.DBTX.QueryRowContext(ctx, query, args...)
}

// TestObservationMutationMatrix proves the five specified counterexamples and
// synthetic unequal-width sequencers against the real migrations and PostgreSQL.
func TestObservationMutationMatrix(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL mutation matrix")
	}
	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	schema := "ic2a_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema+",public")
	require.NoError(t, err)
	migrations, err := filepath.Glob("../../migrations/*.up.sql")
	require.NoError(t, err)
	for _, path := range migrations {
		body, e := os.ReadFile(path)
		require.NoError(t, e)
		// Seed an old row immediately before 000006 to verify NOT NULL backfill.
		if strings.Contains(path, "000006") {
			_, e = tx.ExecContext(ctx, `INSERT INTO file_entries(org_id,bucket_id,storage_path,file_name,size_bytes,status)
    SELECT org_id,id,'legacy','legacy',1,'completed' FROM buckets LIMIT 1`)
			require.NoError(t, e)
		}
		_, e = tx.ExecContext(ctx, strings.ReplaceAll(strings.ReplaceAll(string(body), "BEGIN;", ""), "COMMIT;", ""))
		require.NoError(t, e, path)
	}
	var legacy int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM file_entries WHERE storage_path='legacy' AND observed_at IS NOT NULL").Scan(&legacy)
	require.NoError(t, err)
	require.Equal(t, 1, legacy)
	var bucket, org uuid.UUID
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT id,org_id FROM buckets LIMIT 1").Scan(&bucket, &org))
	var now time.Time
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT now()").Scan(&now))
	names := []string{"A1", "A2", "A3prime", "P2", "A4b", "LPAD"}
	mutants := []string{"baseline", "no_guard", "no_greater", "no_equal", "strict_null", "no_delete_advance", "no_fallback", "no_lpad"}
	expectedKilled := map[string]map[string]bool{
		"no_guard":          {"A1": true, "A3prime": true, "LPAD": true},
		"no_greater":        {"A2": true},
		"no_equal":          {"A3prime": true, "P2": true, "LPAD": true},
		"strict_null":       {"P2": true},
		"no_delete_advance": {"A3prime": true, "LPAD": true},
		"no_fallback":       {"A1": true, "A3prime": true, "A4b": true, "LPAD": true},
		"no_lpad":           {"LPAD": true},
	}
	t.Log("MUTATION | A1 | A2 | A3prime | P2 | A4b | LPAD")
	for _, mutant := range mutants {
		row := []string{mutant}
		for _, name := range names {
			q := New(guardMutation{DBTX: tx, name: mutant})
			p := UpsertIndexedFileParams{OrgID: org, BucketID: bucket, StoragePath: mutant + "/" + name, FileName: name, SizeBytes: 55, Etag: sql.NullString{String: "E1", Valid: true}, Status: FileStatusCompleted, ObservedAt: now, Source: "minio_event", EventSeq: sql.NullString{String: "000000000000009F", Valid: true}}
			apply := func(p UpsertIndexedFileParams) (*FileEntry, error) {
				f, _, e := q.UpsertObservedFile(ctx, p)
				return f, e
			}
			rich := p
			rich.Source = "agent"
			rich.SizeBytes = 100
			rich.Etag.String = "E2"
			rich.Sha256 = sql.NullString{String: "sha-rich", Valid: true}
			rich.EventSeq = sql.NullString{}
			var passed bool
			switch name {
			case "A1", "A4b":
				_, e := apply(rich)
				require.NoError(t, e)
				p.ObservedAt = now.Add(-time.Second)
				f, e := apply(p)
				passed = e == nil
				if name == "A1" {
					passed = passed && f.SizeBytes == 100 && f.Etag.String == "E2"
				}
			case "A2", "P2":
				if name == "A2" {
					p.ObservedAt = now.Add(-time.Second)
				}
				_, e := apply(p)
				require.NoError(t, e)
				f, e := apply(rich)
				passed = e == nil && f.Sha256.String == "sha-rich"
			case "A3prime", "LPAD":
				seqB := "000000000000010A"
				if name == "LPAD" {
					p.EventSeq.String = "9F"
					seqB = "10A"
				}
				_, e := apply(p)
				require.NoError(t, e)
				_, suppressed, found, e := q.MarkObservedFileDeleted(ctx, DeleteIndexedFileParams{BucketID: bucket, StoragePath: p.StoragePath, ObservedAt: now, EventSeq: sql.NullString{String: seqB, Valid: true}, Source: "minio_event"})
				require.NoError(t, e)
				require.True(t, found)
				require.Equal(t, mutant == "no_equal" || (mutant == "no_lpad" && name == "LPAD"), suppressed)
				f, e := apply(p)
				passed = e == nil && f.Status == FileStatusDeleted
			}
			label := "PASS"
			if !passed {
				label = "FAIL"
			}
			row = append(row, label)
			require.Equal(t, !expectedKilled[mutant][name], passed, fmt.Sprintf("%s/%s unexpected mutation result", mutant, name))
		}
		t.Log(strings.Join(row, " | "))
	}
	// Rule-aware writers can complete webhook metadata; event writers preserve it.
	q := New(tx)
	p := UpsertIndexedFileParams{OrgID: org, BucketID: bucket, StoragePath: "metadata", FileName: "metadata", Status: FileStatusCompleted, Source: "minio_event", ObservedAt: now, MetaIncomplete: true}
	f, _, err := q.UpsertObservedFile(ctx, p)
	require.NoError(t, err)
	require.True(t, f.MetaIncomplete)
	p.Source = "agent"
	p.MetaIncomplete = false
	f, _, err = q.UpsertObservedFile(ctx, p)
	require.NoError(t, err)
	require.False(t, f.MetaIncomplete)
	p.MetaIncomplete = true
	f, _, err = q.UpsertObservedFile(ctx, p)
	require.NoError(t, err)
	require.True(t, f.MetaIncomplete)
	p.Source = "minio_event"
	p.MetaIncomplete = false
	p.ObservedAt = now.Add(time.Second)
	f, _, err = q.UpsertObservedFile(ctx, p)
	require.NoError(t, err)
	require.True(t, f.MetaIncomplete)
	_, suppressed, found, err := q.MarkObservedFileDeleted(ctx, DeleteIndexedFileParams{BucketID: bucket, StoragePath: p.StoragePath, ObservedAt: now, Source: "minio_event"})
	require.NoError(t, err)
	require.True(t, suppressed)
	require.True(t, found)
	_, suppressed, found, err = q.MarkObservedFileDeleted(ctx, DeleteIndexedFileParams{BucketID: bucket, StoragePath: "absent", ObservedAt: now, Source: "minio_event"})
	require.NoError(t, err)
	require.False(t, suppressed)
	require.False(t, found)
	// Repeated newer deletions must still advance the fence.
	del := DeleteIndexedFileParams{BucketID: bucket, StoragePath: p.StoragePath, ObservedAt: now.Add(2 * time.Second), Source: "minio_event"}
	_, suppressed, found, err = q.MarkObservedFileDeleted(ctx, del)
	require.NoError(t, err)
	require.False(t, suppressed)
	require.True(t, found)
	del.ObservedAt = now.Add(3 * time.Second)
	f, suppressed, found, err = q.MarkObservedFileDeleted(ctx, del)
	require.NoError(t, err)
	require.False(t, suppressed)
	require.True(t, found)
	require.True(t, f.ObservedAt.Equal(del.ObservedAt))

}
