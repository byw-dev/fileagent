package seaweedfs

// subscribe.go — the subscription loop (design decision D5).
//
// One adapter owns ONE local stream (SubscribeLocalMetadata) and processes it
// strictly in order in a single goroutine: the next response is only read
// after the previous one has been fully applied. That ordering is what makes
// the two core claims true:
//
//   - a persisted idle-heartbeat declared value is a COMPLETENESS claim: every
//     event at or below it was already applied durably when it is saved;
//   - a later read-back always carries a later ObservedAt (D2), so the index's
//     observed_at eviction orders facts correctly.
//
// Failure semantics — an event is NEVER skipped:
//
//   - Apply failure → in-place backoff retry (ApplyMaxRetries), then the
//     stream is abandoned; the resubscribe starts from
//     ResumeFrom(persisted cursor, M) so the failing change is replayed.
//   - Unknown bucket (indexer.ErrUnknownBucket) → NOT a failure: Warn + count
//     + treated as processed, otherwise one strange bucket would wedge the
//     stream forever.
//   - Bucket-directory deletion → never reaches the sink (its semantics are
//     an incident, not an index write): Error log + bucket_removed mark, then
//     the stream moves on.
//   - Cursor/mark persistence failure → stream failure: continuing without a
//     durable cursor would silently lie about completeness.
//
// Shutdown: ctx cancellation returns cleanly; nothing is written on the way
// out (the heartbeat path is the only cursor writer and it runs to completion
// or not at all).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"go.uber.org/zap"
)

// Run drives the adapter until ctx is cancelled: it loads the persisted
// cursor (retrying transient failures with cancellable backoff — a single
// failed read must not permanently stop the event source while the CP keeps
// serving, F2), runs the startup self-check (out-of-range cursor detection,
// bounded by its own deadline), and keeps one subscription stream alive —
// resubscribing with ResumeFrom(persisted cursor, M) whenever the stream
// ends — until ctx is done. It returns only ctx.Err() on shutdown; stream
// and store failures resubscribe/retry forever.
func (a *Adapter) Run(ctx context.Context) error {
	var declared int64
	var found bool
	for {
		var err error
		declared, found, err = a.store.DeclaredCursor(ctx, a.cfg.SourceID)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a.logger.Warn("seaweedfs: loading the persisted cursor failed; retrying (the event source will not start without it)",
			zap.Error(err), zap.String("source_id", a.cfg.SourceID))
		if err := a.sleep(ctx, a.reconnectDelay); err != nil {
			return ctx.Err()
		}
	}
	if !found {
		// Fresh deployment: no cursor row. Subscribe from 0 — the metadata log
		// is kept forever by default and read-back makes the replay
		// idempotent, so a full re-drive is safe (D5).
		a.logger.Info("seaweedfs: no persisted cursor for this source; subscribing since_ns=0 (full replay; read-back makes replay idempotent)",
			zap.String("source_id", a.cfg.SourceID))
	} else {
		a.selfCheck(ctx, declared)
	}
	last := declared // in-memory last declared value; C for gap windows

	for ctx.Err() == nil {
		err := a.runStream(ctx, &last)
		if ctx.Err() != nil {
			return ctx.Err() // clean shutdown: no writes on the way out
		}
		a.logger.Warn("seaweedfs: stream ended; resubscribing",
			zap.Error(err),
			zap.String("source_id", a.cfg.SourceID),
			zap.Int64("persisted_declared", last),
			zap.Int64("next_since_ns", ResumeFrom(last, a.cfg.ResumeMargin)),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.reconnectDelay):
		}
	}
	return ctx.Err()
}

// runStream runs one subscription attempt to completion: dial, subscribe, and
// process responses in order until the stream errors or ctx is done.
// lastDeclared is the adapter's in-memory last declared value (shared across
// attempts); it is advanced only when the corresponding value is durably
// persisted.
func (a *Adapter) runStream(ctx context.Context, lastDeclared *int64) error {
	client, closeConn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("seaweedfs: dial filer: %w", err)
	}
	if closeConn != nil {
		defer closeConn()
	}

	epoch := a.epoch.Add(1) // filer client_epoch: a fresh attempt of the same stable client
	since := ResumeFrom(*lastDeclared, a.cfg.ResumeMargin)
	stream, err := client.SubscribeLocalMetadata(ctx, &filerpb.SubscribeMetadataRequest{
		ClientName:                   a.cfg.ClientName,
		ClientId:                     a.cfg.ClientID,
		ClientEpoch:                  epoch,
		PathPrefix:                   subscribePathPrefix,
		SinceNs:                      since,
		ClientSupportsIdleHeartbeat:  true,  // the declared-value cursor depends on it (D-037 第五条)
		ClientSupportsBatching:       true,  // backlog batches arrive packed in resp.Events
		ClientSupportsMetadataChunks: false, // the client does NOT read log chunks from volume servers
	})
	if err != nil {
		return fmt.Errorf("seaweedfs: subscribe since_ns=%d: %w", since, err)
	}

	a.logger.Info("seaweedfs: subscribed",
		zap.String("source_id", a.cfg.SourceID),
		zap.Int32("client_epoch", epoch),
		zap.Int64("since_ns", since),
		zap.Int64("declared_before", *lastDeclared),
	)

	firstHeartbeat := true
	for {
		resp, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("seaweedfs: recv: %w", err)
		}

		if ts, ok := IsIdleHeartbeat(resp); ok {
			// Idle heartbeat = a completeness claim for everything before it.
			// Strict ordering guarantees those events are all applied.
			if firstHeartbeat {
				firstHeartbeat = false
				if err := a.recordGapWindow(ctx, *lastDeclared, ts); err != nil {
					return err
				}
			}
			if err := a.persistCursor(ctx, lastDeclared, ts); err != nil {
				return err
			}
			continue
		}

		// Flush reports (flushed_ts_ns only) and batches fall through to
		// Translate, which yields nothing for them.
		for _, change := range Translate(resp) {
			if err := a.processChange(ctx, change); err != nil {
				// The change was NOT processed (never skipped): abandon the
				// stream; Run resubscribes from ResumeFrom(persisted, M) so
				// this change is replayed.
				return fmt.Errorf("seaweedfs: process %s %s/%s: %w", change.Kind, change.Bucket, change.Key, err)
			}
		}
	}
}

// processChange turns one Change into durable index state. It implements the
// D5 failure semantics: in-place retry for read-back/Apply failures (the
// change is never skipped), immediate success for unknown buckets, and the
// sink-bypassing bucket-removed handling.
func (a *Adapter) processChange(ctx context.Context, change Change) error {
	if change.Kind == ChangeBucketRemoved {
		return a.handleBucketRemoved(ctx, change)
	}

	var lastErr error
	for attempt := 0; attempt <= a.cfg.ApplyMaxRetries; attempt++ {
		if attempt > 0 {
			if err := a.sleep(ctx, a.cfg.ApplyRetryBackoff); err != nil {
				return err
			}
		}
		event, err := a.readBack(ctx, change)
		if err != nil {
			lastErr = err
			continue
		}
		if err := a.sink.Apply(ctx, event); err != nil {
			if errors.Is(err, indexer.ErrUnknownBucket) {
				// D5: a bucket the CP ledger does not know is not a failure —
				// treating it as one would wedge the stream on the first
				// foreign bucket forever. Warn + count + treat as processed.
				a.unknownBuckets.Add(1)
				a.logger.Warn("seaweedfs: storage event for a bucket unknown to the ledger; treated as processed",
					zap.String("bucket", change.Bucket),
					zap.String("key", change.Key),
					zap.Uint64("unknown_bucket_events", a.unknownBuckets.Load()),
				)
				return nil
			}
			if errors.Is(err, storageevent.ErrSuppressed) && a.reporter != nil {
				// N1: a suppression is only a conflict if the ledger's CURRENT
				// state differs from the fact just read back. A newer
				// observation that recorded EXACTLY this fact (created with
				// the same size/etag, or already deleted) is a confirmation —
				// treat as processed so a hot key cannot starve the stream.
				// A genuine difference re-derives the fact (fresh clock +
				// fresh HEAD) via the retry below.
				st, rerr := a.reporter.RowState(ctx, event.Bucket, event.Key)
				if rerr == nil && sameStateAsFact(event, st) {
					a.logger.Info("seaweedfs: suppressed write confirmed same-state by a newer observation; treated as processed",
						zap.String("bucket", event.Bucket),
						zap.String("key", event.Key),
						zap.String("event_kind", event.Kind.String()),
						zap.String("row_status", st.Status),
					)
					return nil
				}
			}
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("after %d attempts: %w", a.cfg.ApplyMaxRetries+1, lastErr)
}

// sameStateAsFact (N1) reports whether the ledger's current row state already
// records exactly the fact the read-back derived: a created fact whose row is
// completed with the same size and etag, or a removed fact whose row is gone
// or soft-deleted. In both cases a newer observation has confirmed the fact —
// acknowledging it does not lose anything.
func sameStateAsFact(event storageevent.Event, st storageevent.RowState) bool {
	switch event.Kind {
	case storageevent.KindObjectCreated:
		return st.Found && st.Status == "completed" && st.Size == event.Size && st.ETag == event.ETag
	case storageevent.KindObjectRemoved:
		return !st.Found || st.Status == "deleted"
	default:
		return false
	}
}

// handleBucketRemoved implements D5 + D-037 rule 4: a deleted bucket directory
// is an INCIDENT, not an index write. The sink is NOT called (the index sink
// would refuse with ErrUnsupportedKind anyway and the semantics differ — this
// is alert + reconcile, never a ledger mutation); the mark carries the bucket
// and its live-row count so the incident's blast radius is visible to L2.
func (a *Adapter) handleBucketRemoved(ctx context.Context, change Change) error {
	count, err := a.store.CountActiveEntries(ctx, change.Bucket)
	if err != nil {
		return fmt.Errorf("seaweedfs: count ledger rows for removed bucket %s: %w", change.Bucket, err)
	}
	detail, err := json.Marshal(map[string]any{"active_entries": count})
	if err != nil {
		return fmt.Errorf("seaweedfs: marshal bucket-removed detail: %w", err)
	}
	bucket := change.Bucket
	if err := a.store.RecordMark(ctx, Mark{
		SourceID: a.cfg.SourceID,
		Reason:   MarkReasonBucketRemoved,
		Bucket:   &bucket,
		Detail:   detail,
	}); err != nil {
		return fmt.Errorf("seaweedfs: record bucket_removed mark for %s: %w", change.Bucket, err)
	}
	a.logger.Error("seaweedfs: bucket directory removed; if the ledger still has rows for this bucket, this is a D-037 rule-4 ops-bypass incident — ledger left untouched, reconciliation mark written",
		zap.String("bucket", change.Bucket),
		zap.Int64("active_entries", count),
	)
	return nil
}

// recordGapWindow persists the [min(C,H)−M, max(C,H)] window between the
// declared value before this subscription (C) and the first heartbeat after
// it (H). Objects with mtimes inside the window are pending reconciliation —
// PR3b records the WINDOW ONLY (L2 / IC-13 does the per-object scan).
func (a *Adapter) recordGapWindow(ctx context.Context, c, h int64) error {
	from, to := GapWindow(c, h, a.cfg.ResumeMargin)
	err := a.store.RecordMark(ctx, Mark{
		SourceID: a.cfg.SourceID,
		Reason:   MarkReasonGap,
		FromTsNs: int64Ptr(from),
		ToTsNs:   int64Ptr(to),
	})
	if err != nil {
		return fmt.Errorf("seaweedfs: record gap mark: %w", err)
	}
	a.logger.Warn("seaweedfs: stream (re)established; gap window recorded — objects with filer mtimes inside are pending reconciliation (window only; the per-object scan is L2/IC-13)",
		zap.Int64("declared_before", c),
		zap.Int64("first_heartbeat", h),
		zap.Int64("window_from", from),
		zap.Int64("window_to", to),
		zap.String("source_id", a.cfg.SourceID),
	)
	return nil
}

// persistCursor saves the heartbeat's declared value, throttled by
// CursorFlushInterval and never regressing (a completeness claim can only
// move forward). A persistence failure is a stream failure (see file comment).
func (a *Adapter) persistCursor(ctx context.Context, lastDeclared *int64, ts int64) error {
	if ts <= *lastDeclared {
		return nil // never regress
	}
	if !a.lastFlush.IsZero() && time.Since(a.lastFlush) < a.cfg.CursorFlushInterval {
		return nil // throttled; a later heartbeat persists a newer value
	}
	if err := a.store.SaveCursor(ctx, a.cfg.SourceID, ts); err != nil {
		return fmt.Errorf("seaweedfs: persist cursor %d: %w", ts, err)
	}
	*lastDeclared = ts
	a.lastFlush = time.Now()
	return nil
}

// selfCheck implements the startup out-of-range check (D5): with a cursor in
// hand, list the earliest metadata log entry via the filer; a cursor older
// than it means that part of the log was purged (fs.log.purge) and the
// subscription would silently skip it — Error log + a
// cursor_before_earliest_log mark, then startup continues as usual. Any
// failure here is a Warn, never a startup blocker.
func (a *Adapter) selfCheck(ctx context.Context, cursor int64) {
	// The self-check is best-effort: give it its OWN deadline so a filer that
	// accepts the dial but stalls the listing cannot delay the subscription
	// past the timeout (F2). On timeout (or any failure) we Warn and continue.
	ctx, cancel := context.WithTimeout(ctx, a.selfCheckTimeout)
	defer cancel()

	client, closeConn, err := a.dial(ctx)
	if err != nil {
		a.logger.Warn("seaweedfs: startup self-check could not dial the filer; skipping the out-of-range check",
			zap.Error(err), zap.String("source_id", a.cfg.SourceID))
		return
	}
	if closeConn != nil {
		defer closeConn()
	}
	earliest, err := a.listFirstEntry(ctx, client, systemLogDir)
	if err != nil {
		a.logger.Warn("seaweedfs: startup self-check could not list the metadata log dir; skipping the out-of-range check",
			zap.Error(err), zap.String("source_id", a.cfg.SourceID))
		return
	}

	// Two-level layout (D-037 / D5): the log dir holds DAY directories, each
	// holding minute files. The earliest ENTRY is the earliest day's earliest
	// minute file, not the day's midnight — a cursor in between is already out
	// of range and must be detected as such (F2).
	name := earliest
	if !strings.Contains(earliest, "/") {
		if dayEarliest, err := a.listFirstEntry(ctx, client, systemLogDir+"/"+earliest); err != nil {
			a.logger.Warn("seaweedfs: startup self-check could not list the earliest day directory; comparing against the day boundary",
				zap.String("day_dir", earliest), zap.Error(err))
		} else if dayEarliest != "" {
			name = earliest + "/" + dayEarliest
		}
	}

	before, err := CursorBeforeEarliestLog(cursor, name)
	if err != nil {
		a.logger.Warn("seaweedfs: startup self-check could not parse the earliest log name; skipping the out-of-range check",
			zap.String("earliest_log", name), zap.Error(err))
		return
	}
	if !before {
		return
	}
	detail, _ := json.Marshal(map[string]any{"cursor": cursor, "earliest_log": name})
	if err := a.store.RecordMark(ctx, Mark{
		SourceID: a.cfg.SourceID,
		Reason:   MarkReasonCursorBeforeEarliestLog,
		Detail:   detail,
	}); err != nil {
		a.logger.Error("seaweedfs: cursor predates the earliest metadata log file AND the mark could not be written — the purged range will be skipped silently; investigate now",
			zap.Int64("cursor", cursor), zap.String("earliest_log", name), zap.Error(err))
		return
	}
	a.logger.Error("seaweedfs: cursor predates the earliest metadata log file — that range was purged and the subscription would silently skip it; reconciliation mark written, startup continues",
		zap.Int64("cursor", cursor),
		zap.String("earliest_log", name),
		zap.String("source_id", a.cfg.SourceID),
	)
}

// listFirstEntry returns the first entry's name in dir, or "" when the
// directory is empty.
func (a *Adapter) listFirstEntry(ctx context.Context, client FilerClient, dir string) (string, error) {
	stream, err := client.ListEntries(ctx, &filerpb.ListEntriesRequest{
		Directory:  dir,
		Limit:      1, // entries list in lexicographic = chronological order
		OmitChunks: true,
	})
	if err != nil {
		return "", err
	}
	resp, err := stream.Recv()
	if err != nil {
		return "", err
	}
	return resp.GetEntry().GetName(), nil
}

// sleep waits for d, honoring ctx cancellation.
func (a *Adapter) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
