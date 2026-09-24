package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a round-3 (finalization) — PoC gate RED tests.
//
// B-1: the failure count is ONE monotonic series across Redis epochs and PG
// epochs — a Redis outage does not start a new epoch. Redis pre-outage count
// (2) and PG outage count (3) belong to the SAME event, so after recovery the
// next increment must be ≥ 2+3+1=6 — NOT max(redis,pg)=3 (the max model
// counted disjoint epochs as duplicates of each other).
//
// B-2-1: the oversized dedup hash must cover the FULL body (handler-level):
// two oversized payloads sharing the first 8MiB but differing after it must
// get different dedup keys.
//
// B-2-2: oversized raw capture must obey the 64KiB capture cap (assert the
// STORED LENGTH, not just the Truncated flag).
//
// B-2-3: WEBHOOK_MAX_PARSE_BYTES must be a real knob: raising it lets a
// previously-oversized valid payload parse normally (escape hatch works).
// ─────────────────────────────────────────────────────────────────────────────

// TestRED_B1_EpochsAddNotMax: Redis records failures before an outage, PG
// records them during; after Redis recovers the counts must ADD (the series
// is monotonic), not take the max.
func TestRED_B1_EpochsAddNotMax(t *testing.T) {
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
	identity := "b/k/epochs"
	dk := handler.DeadLetterRedisKey(identity)
	t.Cleanup(func() {
		_ = q.ClearWebhookFailCounter(context.Background(), dk)
	})

	// Epoch 1: Redis up — two failures land in Redis (count=2).
	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	client, err := cache.New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	// Epoch 1: two failures via the Redis path (production store).
	epoch1 := handler.NewRedisWebhookFailStore(client, q)
	for i := 0; i < 2; i++ {
		_, err := epoch1.IncrFailCount(context.Background(), identity)
		require.NoError(t, err)
	}

	// Epoch 2: Redis dies (whole Redis instance lost — not just the key, to
	// model the review's Redis-data-loss scenario), PG records 3 failures.
	mr.Close()
	for i := 0; i < 3; i++ {
		_, err := q.IncrWebhookFailCounter(context.Background(), dk)
		require.NoError(t, err)
	}
	pgCount, err := q.GetWebhookFailCounter(context.Background(), dk)
	require.NoError(t, err)
	require.Equal(t, int64(3), pgCount)

	// Recovery: Redis comes back EMPTY (data loss) and the next failure is
	// processed by the production store. The series must ADD: the total is
	// 2 (Redis epoch) + 3 (PG epoch) + 1 (this recovery delivery) = 6.
	// The max model returns max(1, 3) = 3 — swallowing three failures.
	mr2 := miniredis.RunT(t)
	client2, err := cache.New("redis://"+mr2.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client2.Close() })

	store2 := handler.NewRedisWebhookFailStore(client2, q)
	count, incrErr := store2.IncrFailCount(context.Background(), identity)
	require.NoError(t, incrErr)
	assert.GreaterOrEqual(t, count, int64(6),
		"B-1: epochs must ADD (2 Redis + 3 PG + 1 recovery = 6). The max model counted disjoint epochs as duplicates and swallowed 3 failures (got a non-monotonic smaller value)")
}

// TestRED_B2_1_OversizedHashUsesWholeBody_HANDLER: handler-level — two
// oversized payloads sharing the first 8MiB but differing after it must get
// DIFFERENT dedup keys. This goes through readBodyCapped + handler (the
// round-2 test called HashPayload directly and was a fake guard).
func TestRED_B2_1_OversizedHashUsesWholeBody_HANDLER(t *testing.T) {
	fails := newCountingFailStore()

	common := strings.Repeat("a", 9<<20) // > 8MiB
	b1 := `{"filler":"` + common + `tail-ONE","Records":[]}`
	b2 := `{"filler":"` + common + `tail-TWO","Records":[]}`
	require.Greater(t, int64(len(b1)), handler.MaxWebhookParseBytesForTest())

	dead1 := &recordingSink{}
	h1 := ic4aHandler(nil, fails, dead1, 5)
	_ = postIC4A(t, h1, b1)
	require.Equal(t, 1, dead1.count())

	dead2 := &recordingSink{}
	h2 := ic4aHandler(nil, fails, dead2, 5)
	_ = postIC4A(t, h2, b2)
	require.Equal(t, 1, dead2.count())

	assert.NotEqual(t, dead1.last().DedupKey, dead2.last().DedupKey,
		"B-2-1: the dedup hash must cover the FULL body — two oversized payloads sharing only the first 8MiB must not overwrite each other's dead letter (handler-level, through readBodyCapped)")
}

// TestRED_B2_2_OversizedCaptureObey64KiB: the STORED raw payload of an
// oversized dead letter must be ≤ 64KiB (assert the actual length, not just
// the Truncated flag).
func TestRED_B2_2_OversizedCaptureObey64KiB(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 5)

	filler := strings.Repeat("z", 9<<20)
	body := `{"filler":"` + filler + `","Records":[]}`
	w := postIC4A(t, h, body)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, 1, dead.count())

	dl := dead.last()
	assert.LessOrEqual(t, len(dl.RawPayload), 64*1024,
		"B-2-2: oversized raw capture must obey the 64KiB cap (stored %d bytes)", len(dl.RawPayload))
	assert.True(t, dl.Truncated)
}

// TestRED_B2_3_WebhookMaxParseBytesKnobWorks: raising WEBHOOK_MAX_PARSE_BYTES
// must let a previously-oversized payload parse normally (the escape hatch is
// real, not a log fiction).
func TestRED_B2_3_WebhookMaxParseBytesKnobWorks(t *testing.T) {
	t.Setenv("WEBHOOK_MAX_PARSE_BYTES", "16777216") // 16MiB

	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := handler.NewMinioEventHandlerWithPolicy(ix, testWebhookSecret, fails, dead, 5, newTestLogger())
	// The handler must read the raised cap from config (injected via
	// NewMinioEventHandlerWithPolicy → parse cap 16MiB).

	filler := strings.Repeat("z", 9<<20) // > 8MiB default, < 16MiB raised
	body := `{"filler":"` + filler + `","Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"data-sensor"},"object":{"key":"big-but-allowed.csv","size":1,"sequencer":"17KNOB0000000001"}}}]}`
	require.Greater(t, int64(len(body)), handler.MaxWebhookParseBytesForTest(),
		"payload exceeds the default 8MiB cap")
	require.Less(t, int64(len(body)), int64(16<<20), "payload fits the raised 16MiB cap")

	w := postIC4A(t, h, body)
	assert.Equal(t, http.StatusOK, w.Code,
		"B-2-3: with WEBHOOK_MAX_PARSE_BYTES raised, the payload must parse normally")
	require.True(t, ix.called, "the escape hatch must actually work end-to-end")
	assert.Equal(t, 0, dead.count())
}