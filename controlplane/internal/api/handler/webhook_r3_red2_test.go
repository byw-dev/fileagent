package handler_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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