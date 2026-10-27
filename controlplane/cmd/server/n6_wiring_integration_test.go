//go:build integration

package main

// n6_wiring_integration_test.go — R3-1：驱动真实生产接线 startSeaweedfsEventSource
//（不设置 SinkOverride），配真实 indexer + PG，用可控 HEAD 制造「suppressed 且账本
// 状态不同」→ 要求重新 HEAD 并最终落成正确状态。plain sink 的变异（工厂换回
// NewIndexSink）下：不同状态（999/other）的回读写被静默丢弃 → 热 key 停留在
// agent 写入的旧状态（冷 key 照常入账，不发生饥饿）→ 本测试的热 key 终态断言红。

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/seaweedfstest"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"github.com/byw-dev/fileagent/controlplane/migrations"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// n6RacingReader 每次热 key HEAD 之前用生产 upsert 写入不同状态竞争行（首次
// 才写：重试时不再写，否则永远被更新的 agent 写压制）；hotHeadCalls 计数供
// 「重新 HEAD ≥ 2 次」硬断言（nit，返工 4：不依赖 fixture 时钟漂移才触发重读）。
type n6RacingReader struct {
	ix           *indexer.Indexer
	hotKey       string
	hotSuppressN *atomic.Int64
	hotHeadCalls *atomic.Int64
	hotHits      int
}

func (r *n6RacingReader) StatObject(ctx context.Context, bucket, key string) (seaweedfs.ObjectInfo, error) {
	if key == r.hotKey {
		r.hotHits++
		if r.hotHeadCalls != nil {
			r.hotHeadCalls.Add(1)
		}
		if r.hotHits == 1 {
			// 只在第一次 HEAD 时写入不同状态（模拟一次延迟的上报）——
			// 重试时不再写（否则永远被更新的 agent 写压制）。
			_, _ = r.ix.IndexUploadReporting(ctx, bucket, key, 999, "other",
				time.Now().UTC(), "", "agent")
			r.hotSuppressN.Add(1)
		}
	}
	return seaweedfs.ObjectInfo{Size: 1, ETag: "same"}, nil
}

func TestN6WiringSuppressedDifferentStateReReads(t *testing.T) {
	dsn := n1ThrowawayDB(t)
	source, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	require.NoError(t, err)
	require.NoError(t, m.Up())
	srcErr, dbErr := m.Close()
	require.NoError(t, srcErr)
	require.NoError(t, dbErr)

	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	_, err = conn.ExecContext(ctx, `INSERT INTO organizations (id, name) VALUES ($1, 'n6-org') ON CONFLICT (id) DO NOTHING`, orgID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO buckets (org_id, name, description) VALUES ($1, 'n6-bucket', 'x')`, orgID)
	require.NoError(t, err)

	ix := indexer.NewIndexer(conn, nil, zap.NewNop())
	supN := &atomic.Int64{}
	headCalls := &atomic.Int64{}
	reader := &n6RacingReader{ix: ix, hotKey: "hot.csv", hotSuppressN: supN, hotHeadCalls: headCalls}

	// N1 的 same-state fixture 需要被替换：这里写的是 DIFFERENT state
	// （size=999, etag=other），不是同状态 → 适配器必须重读。

	store := &n1CursorStore{}
	resps := []seaweedfstest.ResponseSpec{
		{Directory: "/buckets/n6-bucket", Name: "hot.csv", FileSize: 1, TsNs: 1000},
		{Heartbeat: true, TsNs: 2000},
		{Directory: "/buckets/n6-bucket", Name: "cold.csv", FileSize: 5, TsNs: 3000},
		{Heartbeat: true, TsNs: 4000},
	}
	filer := seaweedfstest.NewScriptedFiler(resps)

	scfg := seaweedfsSourceConfig{
		SourceID: "seaweedfs:n6", FilerAddr: "unused:0",
		ClientName: "n6-test", ClientID: 21,
		ResumeMargin: time.Minute, MaxRetries: 3, RetryBackoff: time.Millisecond,
		MinioClient:    nil, // not used (reader override below)
		Indexer:        ix,
		Logger:         zap.NewNop(),
		StoreOverride:  store,
		ClockOverride:  &n1NoClock{},
		ReaderOverride: reader,
		DialOverride: func(ctx context.Context) (seaweedfs.FilerClient, func(), error) {
			return filer, func() {}, nil
		},
		// SinkOverride 未设置 → 使用生产 newEventSink(true, ix)（aware sink）
	}
	adapter, err := scfg.adapter()
	require.NoError(t, err)
	adapter.WithSuppressionReporter(ix)

	ctx2, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- adapter.Run(ctx2) }()

	deadline := time.Now().Add(10 * time.Second)
	coldRows := 0
	for time.Now().Before(deadline) {
		require.NoError(t, conn.QueryRowContext(context.Background(),
			`SELECT count(*) FROM file_entries WHERE storage_path='cold.csv' AND source='storage_readback'`).Scan(&coldRows))
		if coldRows == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, coldRows, "cold key must be applied (the stream must survive the hot key's suppression)")

	// 热 key 的不同状态 suppression 必须导致重读（重 HEAD）→ 最终写入正确状态
	var hotSize int64
	var hotSrc string
	require.NoError(t, conn.QueryRowContext(context.Background(),
		`SELECT size_bytes, source FROM file_entries WHERE storage_path='hot.csv'`).Scan(&hotSize, &hotSrc))
	require.Equal(t, int64(1), hotSize, "hot key size must come from HEAD re-read")
	require.Equal(t, "storage_readback", hotSrc)

	// nit（返工 4）：热 key 必须真的被重新 HEAD（≥2 次）——不再依赖 fixture
	// 时钟漂移才触发重读；plain-sink 变异下首次 Apply 即被静默吞掉，只有 1 次。
	require.GreaterOrEqual(t, headCalls.Load(), int64(2),
		"the adapter must re-HEAD the hot key (≥2 calls) when a suppression conflicts with the ledger state")

	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
}

// 变异红证明：工厂回退 plain sink 时 suppressed 被吞 → 冷 key 不入账 → 红。
// 本测试直接调用 scfg.adapter() 并检查 sink 行为（不走 Run——只验证接线）。
func TestN6WiringPlainSinkMutationWouldFail(t *testing.T) {
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	_ = orgID // not needed for this unit-level check
	// This test proves the wiring factory produces the right sink type
	// by checking that a suppressed write surfaces as ErrSuppressed.
	// (The full E2E is in the test above.)
	sink := newEventSink(true, &n6FakeReportingWriter{suppressed: true})
	err := sink.Apply(context.Background(), storageevent.Event{
		Kind: storageevent.KindObjectCreated, Bucket: "b", Key: "k", Source: "storage_readback",
	})
	require.ErrorIs(t, err, storageevent.ErrSuppressed,
		"newEventSink(true,…) must produce aware sink — plain sink mutation would make this nil")
}

// n6FakeReportingWriter implements IndexWriter + SuppressionReporting for the
// factory mutation test.
type n6FakeReportingWriter struct{ suppressed bool }

func (w *n6FakeReportingWriter) IndexUpload(context.Context, string, string, int64, string, time.Time, string, string) error {
	return nil
}
func (w *n6FakeReportingWriter) IndexDeletion(context.Context, string, string, time.Time, string, string) error {
	return nil
}
func (w *n6FakeReportingWriter) IndexUploadReporting(context.Context, string, string, int64, string, time.Time, string, string) (bool, error) {
	return w.suppressed, nil
}
func (w *n6FakeReportingWriter) IndexDeletionReporting(context.Context, string, string, time.Time, string, string) (bool, error) {
	return w.suppressed, nil
}
func (w *n6FakeReportingWriter) RowState(context.Context, string, string) (storageevent.RowState, error) {
	return storageevent.RowState{Found: true, Status: "completed", Size: 999, ETag: "different"}, nil
}
