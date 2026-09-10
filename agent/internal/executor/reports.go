package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ReportSender sends a result on the currently connected gRPC stream.
type ReportSender func(*agentv1.AgentMessage) error

// ConfigureReporting sets the transport, acknowledgement deadline and retry limit
// before Start. A missed ack causes report replay, never another object upload.
func (e *Executor) ConfigureReporting(sender ReportSender, timeout time.Duration, retryMax int) error {
	if sender == nil || timeout <= 0 || retryMax < 0 {
		return fmt.Errorf("executor: invalid reporting configuration")
	}
	e.reportSender = sender
	e.reportTimeout = timeout
	e.retryMax = retryMax
	return nil
}

// persistResult stores all metadata before any send can race with an ack.
func (e *Executor) persistResult(ctx context.Context, task *queue.UploadTask, result *uploader.UploadResult, uploadErr error) {
	report := &agentv1.UploadResult{TaskId: task.ID, RuleId: task.RuleID, LocalPath: task.LocalPath, StoragePath: task.StoragePath, Bucket: task.Bucket, SizeBytes: task.FileSize, FileMtime: timestamppb.New(time.Unix(task.FileMtime, 0).UTC()), UploadedAt: timestamppb.Now(), RetryCount: int32(task.RetryCount), Success: uploadErr == nil}
	if result != nil {
		report.Sha256 = result.SHA256
		report.Etag = result.ETag
		report.SizeBytes = result.SizeBytes
		report.StoragePath = result.StoragePath
		report.Bucket = result.Bucket
	}
	if uploadErr != nil {
		report.ErrorMessage = uploadErr.Error()
	}
	payload, err := proto.Marshal(report)
	if err == nil {
		err = e.queue.SaveReport(ctx, task.ID, payload)
	}
	if err != nil {
		e.logger.Error("executor: persist upload report", zap.String("task_id", task.ID), zap.Error(err))
		return
	}
	select {
	case e.reportNotify <- struct{}{}:
	default:
	}
}

// runReporter resumes the durable outbox immediately, then retries expired acks.
func (e *Executor) runReporter(ctx context.Context) {
	defer e.wg.Done()
	timer := time.NewTicker(e.reportTimeout)
	defer timer.Stop()
	for {
		e.sendDueReports(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-e.reportNotify:
		case <-timer.C:
		}
	}
}

// sendDueReports timestamps each attempt before sending and preserves failures.
func (e *Executor) sendDueReports(ctx context.Context) {
	reports, err := e.queue.DueReports(ctx, time.Now().Add(-e.reportTimeout))
	if err != nil {
		e.logger.Warn("executor: list upload reports", zap.Error(err))
		return
	}
	for _, r := range reports {
		var result agentv1.UploadResult
		if err := proto.Unmarshal(r.Payload, &result); err != nil {
			e.logger.Error("executor: decode persisted report", zap.String("task_id", r.TaskID), zap.Error(err))
			continue
		}
		if err := e.queue.TouchReport(ctx, r.TaskID, time.Now()); err != nil {
			e.logger.Warn("executor: report timer", zap.Error(err))
			continue
		}
		if err := e.reportSender(&agentv1.AgentMessage{MessageId: r.TaskID, Payload: &agentv1.AgentMessage_UploadResult{UploadResult: &result}}); err != nil {
			e.logger.Debug("executor: report will retry", zap.String("task_id", r.TaskID), zap.Error(err))
		}
	}
}

// HandleAcknowledgement completes only persisted reports accepted by the CP.
// Duplicate and unrelated acknowledgements are harmless; negative acks retry.
func (e *Executor) HandleAcknowledgement(ctx context.Context, ack *agentv1.Acknowledgement) error {
	if ack == nil || !ack.GetSuccess() {
		return nil
	}
	payload, err := e.queue.GetReport(ctx, ack.GetRefMessageId())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var result agentv1.UploadResult
	if err := proto.Unmarshal(payload, &result); err != nil {
		return err
	}
	return e.queue.CompleteReported(ctx, ack.GetRefMessageId(), result.GetSuccess(), result.GetSha256())
}
