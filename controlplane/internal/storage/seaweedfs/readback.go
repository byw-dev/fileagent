package seaweedfs

// readback.go — D1/D2: an event is only a hint. For every Change the adapter
// re-reads the object's CURRENT state over S3 and turns that into the index
// fact, so the event kind never decides what gets written:
//
//	HEAD 200                  → Created with the HEAD's Size/ETag (the filer's
//	                            reported size is a hint and is IGNORED)
//	HEAD definitive absence   → Removed
//	(NoSuchKey / 404 only)
//	ANY other HEAD error      → processing failure (timeout, 5xx, 403, conn
//	                            refused — never "absent", see TestReadBack_
//	                            OtherHeadError_IsProcessingFailure / mutation ①)
//
// D2 (ObservedAt): the fact's timestamp is a DB clock reading taken BEFORE the
// HEAD is issued. Reasons, in order:
//
//  1. DB clock: agent-written rows take observed_at from the DB's now()
//     (db/queries/ingest.sql); with Seq empty the index evicts stale writes by
//     ObservedAt ALONE, so read-back facts are only comparable to agent facts
//     when they share the DB's clock domain. A CP-local clock Δ behind the DB
//     would let "agent reported, then the object was deleted within Δ" (a
//     Removed that is the key's LAST event, so nothing ever corrects it) be
//     suppressed by the agent row.
//  2. BEFORE the HEAD: the HEAD's view of the object is not older than the
//     moment it was issued, so any later read-back of the same key carries a
//     later timestamp and wins the eviction race — old facts lose to new
//     facts, never the reverse. This only holds because the adapter processes
//     its stream strictly in order (single goroutine, D5).
//
// Known gap (recorded in the PR description): the MinIO webhook path keeps
// using MinIO eventTime — a different clock domain, out of PR3b's scope.

import (
	"context"
	"errors"
	"fmt"

	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
)

// readBack realizes one Change as an index fact (see the file comment for the
// D1/D2 contract). The returned Event carries SourceStorageReadback
// provenance; the caller applies it to the sink (with retry) in processChange.
func (a *Adapter) readBack(ctx context.Context, ch Change) (storageevent.Event, error) {
	// D2: DB clock reading FIRST — before any S3 traffic for this fact.
	observedAt, err := a.clock.Now(ctx)
	if err != nil {
		return storageevent.Event{}, fmt.Errorf("seaweedfs: db clock before HEAD %s/%s: %w", ch.Bucket, ch.Key, err)
	}

	info, err := a.reader.StatObject(ctx, ch.Bucket, ch.Key)
	switch {
	case errors.Is(err, ErrObjectAbsent):
		// Definitive absence — the only path to a Removed fact. A Created
		// change whose object is gone (already deleted, replayed event, …)
		// lands here too: the fact follows the STORAGE, not the hint (D1).
		return storageevent.Event{
			Kind:       storageevent.KindObjectRemoved,
			Bucket:     ch.Bucket,
			Key:        ch.Key,
			ObservedAt: observedAt,
			Source:     storageevent.SourceStorageReadback,
		}, nil
	case err != nil:
		// Timeout, 5xx, 403, connection failure, … — a processing failure.
		// Treating any of these as "absent" would write a wrongful deletion
		// on a storage hiccup (D1; guarded by mutation ①).
		return storageevent.Event{}, fmt.Errorf("seaweedfs: HEAD %s/%s: %w", ch.Bucket, ch.Key, err)
	default:
		return storageevent.Event{
			Kind:       storageevent.KindObjectCreated,
			Bucket:     ch.Bucket,
			Key:        ch.Key,
			Size:       info.Size,
			ETag:       info.ETag,
			ObservedAt: observedAt,
			Source:     storageevent.SourceStorageReadback,
		}, nil
	}
}
