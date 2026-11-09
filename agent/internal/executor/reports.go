package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// inFlightIdentities holds the IC-BUG-48 identity each successful upload
// reported, keyed by task id, until its ack arrives (or the entry is evicted
// by a bounded sweep). It is in-memory by design: identities older than the
// process cannot be vouched for, and a replayed report then stores a zero
// identity → the next candidate check hash-confirms (fail-closed).
type inFlightIdentities struct {
	mu sync.Mutex
	m  map[string]identityEntry
}

type identityEntry struct {
	ident     queue.FileIdentity
	complete  bool
	stampedAt time.Time
}

// recordInFlightIdentity stores the identity of a just-uploaded result.
func (e *Executor) recordInFlightIdentity(taskID string, result *uploader.UploadResult) {
	if e.inFlight == nil || result == nil {
		return
	}
	e.inFlight.mu.Lock()
	defer e.inFlight.mu.Unlock()
	// Bound the map: drop entries older than one hour (acks arrive in
	// seconds; an hour is orders of magnitude beyond any realistic ack lag)
	// and cap at 4096 entries by dropping the oldest — a pathological
	// backlog then degrades to hash confirmation, never to a wrong skip.
	now := time.Now()
	if len(e.inFlight.m) >= 4096 {
		for id, ent := range e.inFlight.m {
			if now.Sub(ent.stampedAt) > time.Hour {
				delete(e.inFlight.m, id)
			}
		}
	}
	e.inFlight.m[taskID] = identityEntry{ident: result.Identity, complete: result.IdentityComplete, stampedAt: now}
}

// takeInFlightIdentity removes and returns the stored identity for a task
// (present only when this process uploaded it and the post-read Fstat
// verified it). ok=false → the ledger stores a zero identity (undecidable →
// hash confirmation).
func (e *Executor) takeInFlightIdentity(taskID string) (queue.FileIdentity, bool) {
	if e.inFlight == nil {
		return queue.FileIdentity{}, false
	}
	e.inFlight.mu.Lock()
	defer e.inFlight.mu.Unlock()
	ent, ok := e.inFlight.m[taskID]
	if !ok {
		return queue.FileIdentity{}, false
	}
	delete(e.inFlight.m, taskID)
	if !ent.complete {
		return queue.FileIdentity{}, false
	}
	return ent.ident, true
}

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

// persistResult stores all metadata before any send can race with an ack. It
// also records the in-flight IC-BUG-48 identity (the uploader's same-fd
// snapshot) so the ack handler can project it into the processed-files
// ledger; complete=false / failed uploads clear it.
func (e *Executor) persistResult(ctx context.Context, task *queue.UploadTask, result *uploader.UploadResult, uploadErr error) {
	report := &agentv1.UploadResult{TaskId: task.ID, RuleId: task.RuleID, LocalPath: task.LocalPath, StoragePath: task.StoragePath, Bucket: task.Bucket, SizeBytes: task.FileSize, FileMtime: timestamppb.New(time.Unix(task.FileMtime, 0).UTC()), UploadedAt: timestamppb.Now(), RetryCount: int32(task.RetryCount), Success: uploadErr == nil}
	if result != nil {
		report.Sha256 = result.SHA256
		report.Etag = result.ETag
		report.SizeBytes = result.SizeBytes
		report.StoragePath = result.StoragePath
		report.Bucket = result.Bucket
	}
	e.recordInFlightIdentity(task.ID, result)
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
//
// The IC-BUG-48 identity travels OUT of band: e.lastIdentity holds the
// UploadResult the executor just persisted (same process, same attempt), so
// the ledger row is written with the uploader's same-fd snapshot. A report
// REPLAYED after a restart finds no in-flight identity and stores a zero one
// — the next candidate check then hash-confirms (fail-closed), which is the
// correct behaviour for an identity nobody can vouch for anymore.
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
	ident, identOK := e.takeInFlightIdentity(ack.GetRefMessageId())
	return e.queue.CompleteReportedIdentity(ctx, ack.GetRefMessageId(), result.GetSuccess(), result.GetSha256(), ident, identOK)
}
