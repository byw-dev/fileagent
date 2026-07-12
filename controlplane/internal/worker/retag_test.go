package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/retag"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mockRetagDB is a hand-rolled RetagJobsDB backed by an in-memory job queue. It
// guards its state with a mutex so the ticker-driven Run tests stay race-free.
type mockRetagDB struct {
	mu    sync.Mutex
	queue []*db.RetagJob // claimed FIFO; ClaimNextRetagJob pops the front

	mergeParams []db.MergeTagValueParams
	mergeAff    int64
	mergeErr    error

	deletedPending []uuid.UUID
	deleteErr      error

	claimErr error

	doneID       uuid.UUID
	doneAffected int32
	doneCalled   bool
	doneCount    int
	doneErr      error

	failedID   uuid.UUID
	failedMsg  string
	failCalled bool

	requeued      int64
	requeueCalled bool
}

func (m *mockRetagDB) ClaimNextRetagJob(_ context.Context) (*db.RetagJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claimErr != nil {
		return nil, m.claimErr
	}
	if len(m.queue) == 0 {
		return nil, sql.ErrNoRows
	}
	job := m.queue[0]
	m.queue = m.queue[1:]
	return job, nil
}
func (m *mockRetagDB) MergeTagValue(_ context.Context, arg db.MergeTagValueParams) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergeParams = append(m.mergeParams, arg)
	return m.mergeAff, m.mergeErr
}
func (m *mockRetagDB) DeletePendingTagValue(_ context.Context, id uuid.UUID, _ uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedPending = append(m.deletedPending, id)
	return 1, m.deleteErr
}
func (m *mockRetagDB) MarkRetagJobDone(_ context.Context, id uuid.UUID, affectedCount int32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doneCalled, m.doneID, m.doneAffected = true, id, affectedCount
	m.doneCount++
	return m.doneErr
}
func (m *mockRetagDB) MarkRetagJobFailed(_ context.Context, id uuid.UUID, lastError sql.NullString) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCalled, m.failedID, m.failedMsg = true, id, lastError.String
	return nil
}

func (m *mockRetagDB) RequeueRunningRetagJobs(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requeueCalled = true
	return m.requeued, nil
}

// doneCalls returns how many jobs were marked done (race-safe for Run tests).
func (m *mockRetagDB) doneCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doneCount
}

// requeueDone reports whether startup requeue ran (race-safe for Run tests).
func (m *mockRetagDB) requeueDone() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requeueCalled
}

func mergeJob(t *testing.T, orgID uuid.UUID, spec retag.MergeSpec) *db.RetagJob {
	t.Helper()
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	return &db.RetagJob{ID: uuid.New(), OrgID: orgID, Kind: retag.KindMerge, Spec: raw, Status: "running"}
}

func TestRetag_DrainOnce_MergeHappyPath(t *testing.T) {
	org, key, pending := uuid.New(), uuid.New(), uuid.New()
	job := mergeJob(t, org, retag.MergeSpec{TagKeyID: key, FromValue: "TYO", ToValue: "tokyo", PendingID: pending})
	m := &mockRetagDB{queue: []*db.RetagJob{job}, mergeAff: 3}
	w := NewRetagWorker(m, zap.NewNop())

	n := w.DrainOnce(context.Background())
	assert.Equal(t, 1, n)

	require.Len(t, m.mergeParams, 1)
	assert.Equal(t, org, m.mergeParams[0].OrgID)
	assert.Equal(t, key, m.mergeParams[0].TagKeyID)
	assert.Equal(t, "TYO", m.mergeParams[0].FromValue)
	assert.Equal(t, "tokyo", m.mergeParams[0].ToValue)
	assert.Equal(t, []uuid.UUID{pending}, m.deletedPending) // queue row dropped
	assert.True(t, m.doneCalled)
	assert.Equal(t, job.ID, m.doneID)
	assert.Equal(t, int32(3), m.doneAffected) // rewritten file count recorded
	assert.False(t, m.failCalled)
}

func TestRetag_DrainOnce_DrainsMultiple(t *testing.T) {
	org := uuid.New()
	jobs := []*db.RetagJob{
		mergeJob(t, org, retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "a", ToValue: "A", PendingID: uuid.New()}),
		mergeJob(t, org, retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "b", ToValue: "B", PendingID: uuid.New()}),
	}
	m := &mockRetagDB{queue: jobs, mergeAff: 1}
	w := NewRetagWorker(m, zap.NewNop())
	assert.Equal(t, 2, w.DrainOnce(context.Background()))
	assert.Len(t, m.mergeParams, 2)
}

func TestRetag_DrainOnce_EmptyQueue(t *testing.T) {
	m := &mockRetagDB{}
	w := NewRetagWorker(m, zap.NewNop())
	assert.Equal(t, 0, w.DrainOnce(context.Background()))
	assert.False(t, m.doneCalled)
	assert.False(t, m.failCalled)
}

func TestRetag_Merge_ExecError_MarksFailed(t *testing.T) {
	org := uuid.New()
	job := mergeJob(t, org, retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "x", ToValue: "y", PendingID: uuid.New()})
	m := &mockRetagDB{queue: []*db.RetagJob{job}, mergeErr: errors.New("db down")}
	w := NewRetagWorker(m, zap.NewNop())

	w.DrainOnce(context.Background())
	assert.True(t, m.failCalled)
	assert.Equal(t, job.ID, m.failedID)
	assert.Contains(t, m.failedMsg, "db down")
	assert.False(t, m.doneCalled)
	assert.Empty(t, m.deletedPending) // pending kept when the rewrite failed
}

func TestRetag_Merge_BadSpec_MarksFailed(t *testing.T) {
	job := &db.RetagJob{ID: uuid.New(), OrgID: uuid.New(), Kind: retag.KindMerge, Spec: json.RawMessage(`{bad`)}
	m := &mockRetagDB{queue: []*db.RetagJob{job}}
	w := NewRetagWorker(m, zap.NewNop())
	w.DrainOnce(context.Background())
	assert.True(t, m.failCalled)
	assert.Empty(t, m.mergeParams) // never reached the rewrite
}

func TestRetag_UnknownKind_MarksFailed(t *testing.T) {
	job := &db.RetagJob{ID: uuid.New(), OrgID: uuid.New(), Kind: "batch_tag", Spec: json.RawMessage(`{}`)}
	m := &mockRetagDB{queue: []*db.RetagJob{job}}
	w := NewRetagWorker(m, zap.NewNop())
	w.DrainOnce(context.Background())
	assert.True(t, m.failCalled)
	assert.Contains(t, m.failedMsg, "unknown retag job kind")
}

func TestRetag_Merge_MarkDoneFails_MarksFailed(t *testing.T) {
	// The merge applied but the bookkeeping write failed. The job must not be left
	// stranded in `running` (never re-claimed) — it is marked failed for visibility.
	org := uuid.New()
	job := mergeJob(t, org, retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "x", ToValue: "y", PendingID: uuid.New()})
	m := &mockRetagDB{queue: []*db.RetagJob{job}, mergeAff: 2, doneErr: errors.New("mark down")}
	w := NewRetagWorker(m, zap.NewNop())
	w.DrainOnce(context.Background())
	assert.True(t, m.failCalled)
	assert.Equal(t, job.ID, m.failedID)
	assert.Contains(t, m.failedMsg, "marking job done failed")
}

func TestRetag_Merge_DeletePendingFails_StillDone(t *testing.T) {
	// Dropping the pending row is best-effort: its failure must not fail the job.
	org := uuid.New()
	job := mergeJob(t, org, retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "x", ToValue: "y", PendingID: uuid.New()})
	m := &mockRetagDB{queue: []*db.RetagJob{job}, mergeAff: 2, deleteErr: errors.New("delete failed")}
	w := NewRetagWorker(m, zap.NewNop())
	w.DrainOnce(context.Background())
	assert.True(t, m.doneCalled)
	assert.False(t, m.failCalled)
}

func TestRetag_DrainOnce_CancelledContext(t *testing.T) {
	m := &mockRetagDB{queue: []*db.RetagJob{mergeJob(t, uuid.New(), retag.MergeSpec{})}}
	w := NewRetagWorker(m, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, 0, w.DrainOnce(ctx)) // stops before claiming
	assert.Empty(t, m.mergeParams)
}

func TestRetag_DrainOnce_ClaimError(t *testing.T) {
	m := &mockRetagDB{claimErr: errors.New("db down")}
	w := NewRetagWorker(m, zap.NewNop())
	assert.Equal(t, 0, w.DrainOnce(context.Background())) // claim failed → nothing processed
	assert.False(t, m.doneCalled)
	assert.False(t, m.failCalled)
}

func TestRetag_Run_StopsOnContextCancel(t *testing.T) {
	m := &mockRetagDB{}
	w := NewRetagWorker(m, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}

func TestRetag_Run_RequeuesStaleOnStartup(t *testing.T) {
	// A job left `running` by a prior crashed process is reset to pending on
	// startup so it is re-claimed rather than stranded forever.
	m := &mockRetagDB{requeued: 1}
	w := NewRetagWorker(m, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, 10*time.Millisecond)
	require.Eventually(t, m.requeueDone, 2*time.Second, 10*time.Millisecond,
		"worker should requeue stale running jobs on startup")
}

func TestRetag_Run_DrainsOnTick(t *testing.T) {
	m := &mockRetagDB{
		queue:    []*db.RetagJob{mergeJob(t, uuid.New(), retag.MergeSpec{TagKeyID: uuid.New(), FromValue: "a", ToValue: "A", PendingID: uuid.New()})},
		mergeAff: 1,
	}
	w := NewRetagWorker(m, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return m.doneCalls() >= 1
	}, 2*time.Second, 10*time.Millisecond, "worker should drain the queued job")
}
