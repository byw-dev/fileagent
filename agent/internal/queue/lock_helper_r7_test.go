package queue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLockHelperTable_NoGrowthAcrossOpens — rework-7 suggestion 5: the
// BEGIN IMMEDIATE lock helper stays EMPTY. The old seed (`INSERT OR IGNORE
// ... VALUES (0)` with no UNIQUE constraint) added one row on every Open
// (the reviewer measured 2 rows after two opens); the no-op UPDATE that
// upgrades the write lock matches zero rows and needs no data.
func TestLockHelperTable_NoGrowthAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	dsn := dir + "/q.db"

	countRows := func() int {
		q, err := Open(dsn)
		require.NoError(t, err)
		defer q.Close()
		var n int
		require.NoError(t, q.db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations_r6_lock`).Scan(&n))
		return n
	}

	require.Equal(t, 0, countRows(), "first Open must not seed the lock helper")
	require.Equal(t, 0, countRows(), "second Open must not add a row (pre-fix: 2)")
	require.Equal(t, 0, countRows(), "third Open must not add a row")
}

// TestEnqueueIfNoActive_LockUpgradeWorksOnEmptyHelper — the empty helper must
// still upgrade the transaction to a write lock (the whole point of the
// table): run the guard's BEGIN IMMEDIATE path against a fresh queue and
// confirm the verdict+insert still work.
func TestEnqueueIfNoActive_LockUpgradeWorksOnEmptyHelper(t *testing.T) {
	q := openMemQueue(t)
	ctx := context.Background()
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42,
		FileCtimeNs: 1, FileDev: 1, FileIno: 2, FileMtimeNs: 1,
		FileSnapAtNs: 1e18}
	ok, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, ok)
}
