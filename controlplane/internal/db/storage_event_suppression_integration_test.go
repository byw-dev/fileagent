//go:build integration

package db

// storage_event_suppression_integration_test.go — formal version of the
// review-r1 F1 probe (TestReviewReadbackSuppressedByDelayedAgent), driven
// against the REAL production queries (SelectDBClockTimestamp /
// UpsertObservedFile / MarkObservedFileDeleted) on a throwaway database.
//
// It pins the F1a contract the read-back adapter relies on:
//
//  1. the phenomenon is real: a read-back delete whose observed_at was taken
//     BEFORE a delayed agent write (agent rows stamp DB now()) is evicted by
//     the upsert guard — suppressed=true, row stays completed/source=agent;
//  2. the resolution works: re-applying the SAME fact with a FRESHER DB clock
//     reading (what the adapter's suppression retry does) evicts the agent
//     row — suppressed=false, row deleted/source=storage_readback.
//
// The adapter-side retry semantics (fresh clock before every HEAD, retry cap)
// are pinned by unit tests in internal/storage/seaweedfs (suppression_test.go).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestReadbackSuppressedDeleteIsRetriableWithFresherClock(t *testing.T) {
	dsn := storageEventTestDB(t)
	migrateTestDB(t, dsn, +1)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()
	q := New(conn)

	// Seed the default org and a bucket, then create the object row the way
	// the adapter would have (a read-back Created fact, stamped by the DB).
	require.NoError(t, tryInsertSource(t, conn, "storage_readback"))
	var bucketID uuid.UUID
	var key = "probe/suppression-race"
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT bucket_id FROM file_entries WHERE storage_path = 'probe/storage_readback' LIMIT 1`).Scan(&bucketID))
	key = "probe/suppression-race" // the row tryInsertSource created

	// T1: the adapter's DB clock reading, taken BEFORE its HEAD (decision D2).
	t1, err := q.SelectDBClockTimestamp(ctx)
	require.NoError(t, err)

	// The delayed agent UploadResult lands while the HEAD is in flight: agent
	// rows stamp DB now(), which is strictly after T1.
	time.Sleep(2 * time.Millisecond)
	agentWrittenAt, err := q.SelectDBClockTimestamp(ctx)
	require.NoError(t, err)
	require.True(t, agentWrittenAt.After(t1), "test setup: the delayed agent write must postdate T1")
	_, err = q.UpsertIndexedFile(ctx, UpsertIndexedFileParams{
		OrgID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		BucketID:     bucketID,
		StoragePath:  key,
		FileName:     "suppression-race",
		SizeBytes:    10,
		Source:       "agent",
		Status:       FileStatusCompleted,
		ObservedAt:   agentWrittenAt,
		UploadedAt:   sql.NullTime{Time: agentWrittenAt, Valid: true},
		MetaIncomplete: false,
	})
	require.NoError(t, err)

	// The HEAD now definitively reports 404; the adapter applies the Removed
	// fact carrying T1 — it MUST be evicted (suppressed), not applied.
	_, suppressed, found, err := q.MarkObservedFileDeleted(ctx, DeleteIndexedFileParams{
		BucketID: bucketID, StoragePath: key, ObservedAt: t1, Source: "storage_readback",
	})
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, suppressed, "the stale read-back delete must be evicted by the newer agent row (the F1 phenomenon)")

	var status, source string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status, source FROM file_entries WHERE storage_path = $1`, key).Scan(&status, &source))
	require.Equal(t, "completed", status, "phenomenon: the row survived the stale delete")
	require.Equal(t, "agent", source)

	// F1a resolution: the adapter re-derives the fact with a FRESH DB clock
	// reading (taken before its fresh HEAD). That reading postdates the agent
	// write, so the same fact now evicts the agent row.
	t3, err := q.SelectDBClockTimestamp(ctx)
	require.NoError(t, err)
	require.True(t, t3.After(agentWrittenAt), "test setup: the fresh reading must postdate the delayed agent write")
	_, suppressedRetry, foundRetry, err := q.MarkObservedFileDeleted(ctx, DeleteIndexedFileParams{
		BucketID: bucketID, StoragePath: key, ObservedAt: t3, Source: "storage_readback",
	})
	require.NoError(t, err)
	require.True(t, foundRetry)
	require.False(t, suppressedRetry, "the retried delete carries a fresher clock and must win")

	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT status, source FROM file_entries WHERE storage_path = $1`, key).Scan(&status, &source))
	require.Equal(t, "deleted", status, "the retried read-back delete must land (the key's last event is not lost)")
	require.Equal(t, "storage_readback", source)
}
