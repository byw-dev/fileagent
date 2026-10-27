package main

// storage_event_source.go — SeaweedFS 事件源的接线工厂（返工 2，N2/N6）。
// main.go 只调用这里；本文件的函数可在单测中直接驱动，守护两件事：
//   1. SeaweedFS 配置必须产出 suppression-aware sink（N6：回退 plain sink 的
//      变异会被 wiring 测试抓红）；
//   2. startSeaweedfsEventSource 返回的 done channel 在 goroutine 退出时关闭，
//      main 的 shutdown join 用 awaitEventSourceStop 等待它（N2：:= 遮蔽曾让
//      join 永不执行——现在 main 显式赋值给外层声明的变量）。

import (
	"context"
	"errors"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	miniogo "github.com/minio/minio-go/v7"
	"go.uber.org/zap"
)

// seaweedfsSourceConfig 是 startSeaweedfsEventSource 的入参（main 的 cfg 子集 +
// 已构建依赖 + 测试注入钩子）。
type seaweedfsSourceConfig struct {
	SourceID     string
	FilerAddr    string
	ClientName   string
	ClientID     int32
	ResumeMargin time.Duration
	MaxRetries   int
	RetryBackoff time.Duration

	MinioClient *miniogo.Client
	Indexer     *indexer.Indexer
	Queries     *db.Queries
	Logger      *zap.Logger

	// 测试注入钩子（nil → 生产实现）。
	StoreOverride  seaweedfs.CursorMarkStore
	ClockOverride  seaweedfs.DBClock
	DialOverride   seaweedfs.DialFunc
	SinkOverride   storageevent.Sink
	ReaderOverride seaweedfs.ObjectReader
}

// newEventSink 按事件源选择 sink（N6 守护点）：SeaweedFS → suppression-aware
// （suppressed 写入返回 ErrSuppressed，N1）；MinIO → plain（行为不变）。
func newEventSink(seaweedfsEnabled bool, ix storageevent.IndexWriter) storageevent.Sink {
	if seaweedfsEnabled {
		return storageevent.NewSuppressionAwareIndexSink(ix)
	}
	return storageevent.NewIndexSink(ix)
}

// adapter 按 cfg 构建 adapter（含测试注入钩子的解析）。
func (c *seaweedfsSourceConfig) adapter() (*seaweedfs.Adapter, error) {
	store := c.StoreOverride
	if store == nil {
		store = &storageEventStore{q: c.Queries}
	}
	clock := c.ClockOverride
	if clock == nil {
		clock = &dbClock{q: c.Queries}
	}
	dial := c.DialOverride
	if dial == nil {
		dial = seaweedfs.NewGRPCDialer(c.FilerAddr)
	}
	sink := c.SinkOverride
	if sink == nil {
		sink = newEventSink(true, c.Indexer)
	}
	reader := c.ReaderOverride
	if reader == nil {
		reader = &minioObjectReader{client: c.MinioClient}
	}
	return seaweedfs.NewAdapter(
		seaweedfs.AdapterConfig{
			SourceID:          c.SourceID,
			ClientName:        c.ClientName,
			ClientID:          c.ClientID,
			ResumeMargin:      c.ResumeMargin,
			ApplyMaxRetries:   c.MaxRetries,
			ApplyRetryBackoff: c.RetryBackoff,
		},
		dial, reader, store, clock, sink, c.Logger,
	)
}

// startSeaweedfsEventSource 构建 adapter 并在后台 goroutine 里运行；返回的
// channel 在 goroutine 退出时关闭（main 的 shutdown join 等待它，N2）。
func startSeaweedfsEventSource(ctx context.Context, cfg seaweedfsSourceConfig) (<-chan struct{}, error) {
	adapter, err := cfg.adapter()
	if err != nil {
		return nil, err
	}
	adapter.WithSuppressionReporter(cfg.Indexer)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := adapter.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			cfg.Logger.Error("storage event source adapter stopped with an error", zap.Error(err))
		}
	}()
	return done, nil
}

// awaitEventSourceStop 等待事件源 goroutine 退出（N2 的 join 语义）。
// done == nil（适配器未启用）视为已停止。
func awaitEventSourceStop(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
