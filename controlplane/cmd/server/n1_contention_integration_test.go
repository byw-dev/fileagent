//go:build integration

package main

// n1_contention_integration_test.go — formal version of review-r2's
// r2-contention probe (N1): a hot key whose read-back is raced by a
// same-state delayed agent write (clock T1 → agent write T2>T1 with the same
// size/etag → HEAD) must NOT starve the stream. With the N1 fix the
// same-state suppression is acknowledged (processed) and the cold key + the
// following heartbeat advance; without the fix the hot key exhausts retries,
// the stream breaks, and the cold key/heartbeat are never consumed.
//
// Lives in cmd/server: the only package allowed to wire the REAL chain across
// modules (seaweedfs adapter + indexer + real PG, D-037 rule 2 / D7).

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/seaweedfstest"
	"github.com/byw-dev/fileagent/controlplane/migrations"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func n1ThrowawayDB(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the N1 contention integration test")
	}
	admin, err := sql.Open("postgres", strings.Replace(dsn, "/fileagent_test", "/postgres", 1))
	require.NoError(t, err)
	name := "fa_n1_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf("CREATE DATABASE %s", name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", name))
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		if cerr := admin.Close(); cerr != nil {
			t.Errorf("close admin: %v", cerr)
		}
	})
	i := strings.LastIndex(dsn, "/")
	return dsn[:i+1] + name + "?sslmode=disable"
}

// racingReader: the hot key's HEAD performs the competing delayed agent write
// (same size/etag as the HEAD fact) right before answering.
type racingReader struct {
	ix      *indexer.Indexer
	bucket  string
	hotKey  string
	hotHits int
}

func (r *racingReader) StatObject(ctx context.Context, bucket, key string) (seaweedfs.ObjectInfo, error) {
	if key == r.hotKey {
		r.hotHits++
		if r.hotHits == 1 {
			// 只在第一次 HEAD 时写入同状态（模拟一次延迟的上报）。
			_, _ = r.ix.IndexUploadReporting(ctx, bucket, key, 1, "same",
				time.Now().UTC(), "", "agent")
		}
	}
	return seaweedfs.ObjectInfo{Size: 1, ETag: "same"}, nil
}

// n1CursorStore records saved cursors (mutex-protected for -race, R3-2).
type n1CursorStore struct {
	mu    sync.Mutex
	Saves []int64
}

func (s *n1CursorStore) DeclaredCursor(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}
func (s *n1CursorStore) SaveCursor(_ context.Context, _ string, v int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Saves = append(s.Saves, v)
	return nil
}
func (s *n1CursorStore) MaxSaved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m int64
	for _, v := range s.Saves {
		if v > m {
			m = v
		}
	}
	return m
}
func (s *n1CursorStore) RecordMark(context.Context, seaweedfs.MarkAlias) error { return nil }
func (s *n1CursorStore) CountActiveEntries(context.Context, string) (int64, error) {
	return 0, nil
}

func TestN1HotKeySameStateContentionDoesNotStarveStream(t *testing.T) {
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
	_, err = conn.ExecContext(ctx, `INSERT INTO organizations (id, name) VALUES ($1, 'n1-org') ON CONFLICT (id) DO NOTHING`, orgID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO buckets (org_id, name, description) VALUES ($1, 'n1-bucket', 'x')`, orgID)
	require.NoError(t, err)

	// 生产链路（R3-1）：走 startSeaweedfsEventSource 的同一工厂 scfg.adapter()，
	// 不设 SinkOverride —— 工厂回退 plain sink 的变异必须被本测试抓红。
	ix := indexer.NewIndexer(conn, nil, zap.NewNop())
	reader := &racingReader{ix: ix, bucket: "n1-bucket", hotKey: "hot.csv"}
	store := &n1CursorStore{}

	// 流：热 key（同状态竞争）→ 心跳 → 冷 key → 心跳。
	resps := []seaweedfstest.ResponseSpec{
		{Directory: "/buckets/n1-bucket", Name: "hot.csv", FileSize: 1, TsNs: 1000},
		{Heartbeat: true, TsNs: 2000},
		{Directory: "/buckets/n1-bucket", Name: "cold.csv", FileSize: 5, TsNs: 3000},
		{Heartbeat: true, TsNs: 4000},
	}
	filer := seaweedfstest.NewScriptedFiler(resps)

	logger, logs := n1TestLogger(t)
	scfg := seaweedfsSourceConfig{
		SourceID: "seaweedfs:n1", FilerAddr: "unused:0",
		ClientName: "n1-test", ClientID: 11,
		ResumeMargin: time.Minute, MaxRetries: 3, RetryBackoff: time.Millisecond,
		Indexer:       ix,
		Logger:        logger,
		StoreOverride: store,
		ClockOverride: &n1NoClock{},
		ReaderOverride: reader,
		DialOverride: func(ctx context.Context) (seaweedfs.FilerClient, func(), error) {
			return filer, func() {}, nil
		},
		// SinkOverride 未设置 → 生产 newEventSink(true, ix)（aware sink，N6 守护点）。
	}
	adapter, err := scfg.adapter()
	require.NoError(t, err)
	adapter.WithSuppressionReporter(ix)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- adapter.Run(ctx) }()

	// 修复后：同状态竞争被确认 → 心跳 2000 的游标保存 → 冷 key 入账。轮询等待（10s）。
	// 游标是硬断言（R3-1）：截止时间内 MaxSaved 必须越过热 key 之后的目标心跳 2000
	//（饥饿时流断在热 key 上，2000 永远不会被处理，游标永远停在初始值）。
	// 末尾心跳 4000 不作断言点：生产接线的 CursorFlushInterval 是默认 1s 节流，
	// 游标只在心跳上保存，4000 若被节流跳过则之后没有任何心跳再触发补写——
	// 它不是确定可达的断言（饥饿检测由 2000 + 冷 key 共同保证）。
	deadline := time.Now().Add(10 * time.Second)
	coldRows, saves := 0, int64(0)
	for time.Now().Before(deadline) {
		require.NoError(t, conn.QueryRowContext(context.Background(),
			`SELECT count(*) FROM file_entries WHERE storage_path='cold.csv' AND source='storage_readback'`).Scan(&coldRows))
		saves = store.MaxSaved()
		if coldRows == 1 && saves >= 2000 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if coldRows != 1 || saves < 2000 {
		for _, e := range logs.All() {
			t.Logf("adapter: %s %v", e.Message, e.Context)
		}
	}
	require.Equal(t, 1, coldRows,
		"cold key must be applied after the hot key's same-state suppression is acknowledged (pre-fix: the stream starves and this times out)")
	require.GreaterOrEqual(t, saves, int64(2000),
		"cursor must pass the target heartbeat (2000) that follows the contended hot key — a starved stream never reaches it")

	// N1 的 same-state 确认必须真的发生：plain-sink 变异下同状态写被静默丢弃
	//（无 ErrSuppressed → 无确认日志）→ 本断言红；aware 路径绿。
	confirmed := false
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "confirmed same-state") {
			confirmed = true
			break
		}
	}
	require.True(t, confirmed,
		"production wiring must yield the suppression-aware sink so the same-state contention is explicitly confirmed (a plain sink drops the write silently)")
	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
}

func n1TestLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

type n1NoClock struct{}

func (c *n1NoClock) Now(context.Context) (time.Time, error) {
	return time.Now(), nil
}
