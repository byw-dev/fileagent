package handler_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// Round-3 S-1: stale counter cleanup must actually RUN (startup + periodic),
// otherwise webhook_fail_counters grows unboundedly with historical failure
// identities. RED test: seed a stale row (updated_at older than the TTL),
// run the exported cleanup entrypoint, assert it's gone — and that fresh
// rows survive.
// ─────────────────────────────────────────────────────────────────────────────

func TestS1_StaleCounterCleanup(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@127.0.0.1:5432/fileagent?sslmode=disable"
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	q := db.New(conn)

	staleDK := handler.DeadLetterRedisKey("b/k/stale-cleanup")
	freshDK := handler.DeadLetterRedisKey("b/k/fresh-cleanup")
	t.Cleanup(func() {
		_, _ = conn.Exec("DELETE FROM webhook_fail_counters WHERE dedup_key IN ($1, $2)", staleDK, freshDK)
	})
	_, _ = conn.Exec("DELETE FROM webhook_fail_counters WHERE dedup_key IN ($1, $2)", staleDK, freshDK)

	// Seed a fresh row via the normal path, then force its updated_at to be
	// older than the TTL (simulating age).
	_, err = q.IncrWebhookFailCounter(context.Background(), staleDK)
	require.NoError(t, err)
	_, err = q.IncrWebhookFailCounter(context.Background(), freshDK)
	require.NoError(t, err)
	old := time.Now().Add(-8 * 24 * time.Hour)
	_, err = conn.Exec("UPDATE webhook_fail_counters SET updated_at=$1 WHERE dedup_key=$2", old, staleDK)
	require.NoError(t, err)

	// The exported cleanup entrypoint (does not exist yet — RED).
	deleted, err := handler.CleanupStaleWebhookFailCounters(context.Background(), q, 7*24*time.Hour)
	require.NoError(t, err)

	t.Logf("DEBUG deleted=%d", deleted)
	// A deleted row legitimately no longer exists — probe existence instead
	// of requiring a value.
	rowExists := func(dk string) bool {
		var c int64
		err := conn.QueryRow("SELECT count FROM webhook_fail_counters WHERE dedup_key=$1", dk).Scan(&c)
		return err == nil
	}
	assert.Equal(t, int64(1), deleted, "the stale row must be reported as deleted")
	assert.False(t, rowExists(staleDK), "rows older than the TTL must be removed (the migration's lifecycle claim must be true)")
	assert.True(t, rowExists(freshDK), "fresh rows must survive the cleanup")
}

// TestFailCount_CacheMissFallsThroughToPG: a Redis cache miss (key expired or
// lost) must NOT regress the series — the authoritative PG count is returned.
func TestFailCount_CacheMissFallsThroughToPG(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@127.0.0.1:5432/fileagent?sslmode=disable"
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	q := db.New(conn)
	identity := "b/k/cachemiss"
	dk := handler.DeadLetterRedisKey(identity)
	_, _ = conn.Exec("DELETE FROM webhook_fail_counters WHERE dedup_key=$1", dk)
	t.Cleanup(func() { _ = q.ClearWebhookFailCounter(context.Background(), dk) })

	// Authoritative count = 3 (PG), cache empty.
	for i := 0; i < 3; i++ {
		_, err := q.IncrWebhookFailCounter(context.Background(), dk)
		require.NoError(t, err)
	}

	// Redis fresh + empty: the read must fall through to PG (3), never 0.
	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	client, err := cache.New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	store := handler.NewRedisWebhookFailStore(client, q)

	count, err := store.FailCount(context.Background(), identity)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count,
		"a cache miss must fall through to the authoritative PG count — the series never regresses")

	// The read path is read-only: the cache is repopulated by the next
	// IncrFailCount (the write path owns cache refreshes).
}

// TestRunStaleCounterCleanup_RunsAndStops: the background sweeper must run
// its first sweep immediately (startup hygiene — S-1) and exit on ctx cancel.
func TestRunStaleCounterCleanup_RunsAndStops(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@127.0.0.1:5432/fileagent?sslmode=disable"
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("no PostgreSQL: %v", err)
	}
	q := db.New(conn)
	dk := handler.DeadLetterRedisKey("b/k/sweep-runner")
	_, _ = conn.Exec("DELETE FROM webhook_fail_counters WHERE dedup_key=$1", dk)
	t.Cleanup(func() { _ = q.ClearWebhookFailCounter(context.Background(), dk) })

	// Seed a stale row, then let the runner's immediate sweep delete it.
	_, err = q.IncrWebhookFailCounter(context.Background(), dk)
	require.NoError(t, err)
	old := time.Now().Add(-8 * 24 * time.Hour)
	_, err = conn.Exec("UPDATE webhook_fail_counters SET updated_at=$1 WHERE dedup_key=$2", old, dk)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handler.RunStaleCounterCleanup(ctx, q, 7*24*time.Hour, time.Hour, newTestLogger())
		close(done)
	}()
	// The immediate sweep is synchronous at runner start; a short wait makes
	// the assertion deterministic without sleeping the full interval.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var exists bool
		err := conn.QueryRow("SELECT EXISTS(SELECT 1 FROM webhook_fail_counters WHERE dedup_key=$1)", dk).Scan(&exists)
		require.NoError(t, err)
		if !exists {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var exists bool
	require.NoError(t, conn.QueryRow("SELECT EXISTS(SELECT 1 FROM webhook_fail_counters WHERE dedup_key=$1)", dk).Scan(&exists))
	assert.False(t, exists, "the runner's startup sweep must delete stale rows (S-1 lifecycle must be real)")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner must exit when ctx is cancelled")
	}
	assert.True(t, handler.WebhookFailCounterTTL() > 0)
}
