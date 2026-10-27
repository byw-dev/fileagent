package seaweedfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
)

// ── N1: 同状态 suppression 是确认，不是冲突 ─────────────────────────────────
//
// 竞争时序：回读取钟 T1 → 迟到的 agent/API 写入以 T2>T1 落账（同 size/etag）→
// HEAD 看到的事实与账本行完全一致 → upsert 驱逐本次写入（suppressed）。此时
// 「一个更新的观察已经记录了同一事实」——应当视为已处理（不重试、不断流），
// 让热 key 后面的冷 key 与心跳照常推进。只有账本状态与 HEAD 事实**不同**才是
// 真冲突，才重新取时钟 + 重新 HEAD（N1 修复的第二半）。

// fakeSuppressionReporter scripts RowState answers.
type fakeSuppressionReporter struct {
	state storageevent.RowState
	err   error
	calls int
}

func (f *fakeSuppressionReporter) IndexUploadReporting(context.Context, string, string, int64, string, time.Time, string, string) (bool, error) {
	return false, nil
}

func (f *fakeSuppressionReporter) IndexDeletionReporting(context.Context, string, string, time.Time, string, string) (bool, error) {
	return false, nil
}

func (f *fakeSuppressionReporter) RowState(context.Context, string, string) (storageevent.RowState, error) {
	f.calls++
	if f.err != nil {
		return storageevent.RowState{}, f.err
	}
	return f.state, nil
}

var (
	_ storageevent.SuppressionReporting = (*fakeSuppressionReporter)(nil)
)

// sameStateAsFact：四种判定分支。
func TestSameStateAsFact(t *testing.T) {
	created := storageevent.Event{Kind: storageevent.KindObjectCreated, Bucket: "b", Key: "k", Size: 223, ETag: "e1"}
	removed := storageevent.Event{Kind: storageevent.KindObjectRemoved, Bucket: "b", Key: "k"}

	if !sameStateAsFact(created, storageevent.RowState{Found: true, Status: "completed", Size: 223, ETag: "e1"}) {
		t.Error("created：行 completed + 同 size/etag 应判同状态")
	}
	if sameStateAsFact(created, storageevent.RowState{Found: true, Status: "completed", Size: 999, ETag: "e1"}) {
		t.Error("created：size 不同不是同状态（真冲突，需重读）")
	}
	if !sameStateAsFact(removed, storageevent.RowState{Found: false}) {
		t.Error("removed：行不存在应判同状态")
	}
	if !sameStateAsFact(removed, storageevent.RowState{Found: true, Status: "deleted"}) {
		t.Error("removed：行已删除应判同状态")
	}
	if sameStateAsFact(removed, storageevent.RowState{Found: true, Status: "completed"}) {
		t.Error("removed：行还活着不是同状态（真冲突，需重读）")
	}
}

// 同状态 suppression：一次 Apply 后直接视为已处理（不再重试时钟/HEAD），
// 流继续——热 key 不阻塞后续处理。
func TestProcessChange_SuppressedSameState_TreatedAsProcessed(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 223, ETag: "same"}}}
	clock := &fakeClock{}
	sink := &scriptedSink{outcomes: []error{storageevent.ErrSuppressed}}
	reporter := &fakeSuppressionReporter{state: storageevent.RowState{Found: true, Status: "completed", Size: 223, ETag: "same"}}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)
	a.WithSuppressionReporter(reporter)

	if err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"}); err != nil {
		t.Fatalf("processChange = %v, want nil（同状态确认 = 已处理）", err)
	}
	if got := a.sinkAppliedCount(sink); got != 1 {
		t.Errorf("Apply attempts = %d, want 1（不重试）", got)
	}
	if got := clock.readingCount(); got != 1 {
		t.Errorf("clock readings = %d, want 1（同状态不需要重取时钟）", got)
	}
	if reporter.calls != 1 {
		t.Errorf("RowState calls = %d, want 1", reporter.calls)
	}
}

// 状态不同的 suppression：真冲突 → 重新取时钟 + 重新 HEAD + 重新 Apply。
func TestProcessChange_SuppressedDifferentState_ReReads(t *testing.T) {
	seq := 0
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 223, ETag: "new"}}, seq: &seq}
	clock := &fakeClock{seq: &seq}
	sink := &scriptedSink{outcomes: []error{storageevent.ErrSuppressed}}
	reporter := &fakeSuppressionReporter{state: storageevent.RowState{Found: true, Status: "completed", Size: 999, ETag: "other"}}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)
	a.WithSuppressionReporter(reporter)

	if err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"}); err != nil {
		t.Fatalf("processChange = %v, want nil（重读后成功）", err)
	}
	if a.sinkAppliedCount(sink) != 2 {
		t.Fatalf("Apply attempts = %d, want 2（真冲突必须重推导）", a.sinkAppliedCount(sink))
	}
	if clock.calls[1] != "clock@3" || reader.calls[1] != "head@4" {
		t.Errorf("重读必须重新取时钟（HEAD 前）+ 重新 HEAD，got %v / %v", clock.calls, reader.calls)
	}
}

// RowState 查询失败：退回旧行为（按真冲突重试，不能静默当同状态）。
func TestProcessChange_SuppressedRowStateError_Retries(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 223, ETag: "same"}}}
	clock := &fakeClock{}
	sink := &scriptedSink{outcomes: []error{
		storageevent.ErrSuppressed, storageevent.ErrSuppressed, storageevent.ErrSuppressed, storageevent.ErrSuppressed,
	}}
	reporter := &fakeSuppressionReporter{err: errors.New("db down")}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)
	a.WithSuppressionReporter(reporter)

	err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if !errors.Is(err, storageevent.ErrSuppressed) {
		t.Fatalf("processChange = %v, want ErrSuppressed after cap（RowState 错误 → 按真冲突重试）", err)
	}
	if clock.readingCount() != 4 {
		t.Errorf("clock readings = %d, want 4（每次重试都重新取时钟/HEAD）", clock.readingCount())
	}
}

// 未接线 reporter（nil）：保持 F1a 行为（一律重试），不 panic。
func TestProcessChange_NilReporter_KeepsLegacyRetry(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 223, ETag: "same"}}}
	clock := &fakeClock{}
	sink := &scriptedSink{outcomes: []error{
		storageevent.ErrSuppressed, storageevent.ErrSuppressed, storageevent.ErrSuppressed, storageevent.ErrSuppressed,
	}}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)

	err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if !errors.Is(err, storageevent.ErrSuppressed) {
		t.Fatalf("processChange = %v, want ErrSuppressed after cap（nil reporter = 旧语义）", err)
	}
}
