package executor

import (
	"context"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"sync/atomic"
	"testing"
	"time"
)

// TestReportingLostAckRetriesOnlyResult injects a lost first ack and no reupload.
func TestReportingLostAckRetriesOnlyResult(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)
	task := newTask("rule", "file")
	require.NoError(t, q.Enqueue(task))
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	e.processTask(ctx, tasks[0])
	reported, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	require.Len(t, reported, 1)
	var sends atomic.Int32
	require.NoError(t, e.ConfigureReporting(func(m *agentv1.AgentMessage) error {
		if sends.Add(1) == 1 {
			return nil
		}
		return e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: m.GetUploadResult().GetTaskId(), Success: true})
	}, 10*time.Millisecond, 1))
	e.Start(ctx)
	defer e.Stop()
	require.Eventually(t, func() bool { rows, _ := q.ListByStatus(queue.StatusCompleted); return len(rows) == 1 }, time.Second, time.Millisecond)
	require.GreaterOrEqual(t, sends.Load(), int32(2))
	require.NoError(t, e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: task.ID, Success: true}))
	require.NoError(t, e.HandleAcknowledgement(ctx, nil))
	require.NoError(t, e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: task.ID, Success: false}))
}

// TestReportingFailurePayloadAndTransportFailure keeps exhausted uploads retryable.
func TestReportingFailurePayloadAndTransportFailure(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	e := New(1, q, failUploader, zap.NewNop(), 0)
	require.Error(t, e.ConfigureReporting(nil, 0, -1))
	var sends int
	require.NoError(t, e.ConfigureReporting(func(m *agentv1.AgentMessage) error {
		sends++
		require.False(t, m.GetUploadResult().GetSuccess())
		return context.Canceled
	}, time.Millisecond, 0))
	task := newTask("rule", "file")
	require.NoError(t, q.Enqueue(task))
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	e.processTask(ctx, tasks[0])
	payload, err := q.GetReport(ctx, task.ID)
	require.NoError(t, err)
	var result agentv1.UploadResult
	require.NoError(t, proto.Unmarshal(payload, &result))
	require.False(t, result.Success)
	require.NotEmpty(t, result.ErrorMessage)
	e.sendDueReports(ctx)
	require.Equal(t, 1, sends)
	rows, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: task.ID, Success: true}))
	processed, err := q.IsProcessed(context.Background(), task.RuleID, task.LocalPath, task.FileMtime, task.FileSize)
	require.NoError(t, err)
	require.False(t, processed)
	require.NoError(t, q.Close())
	require.Error(t, e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: "x", Success: true}))
	e.sendDueReports(ctx)
}
