package executor

import (
	"context"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestInFlightCache_BoundedAt4096_EvictedAckWritesZeroIdentity — rework-7
// fix 4: the in-flight identity cache has a REAL capacity bound. Inserting
// 4096+N brand-new entries must leave the map at ≤4096 (the old sweep only
// deleted hour-old entries, so a burst of fresh entries grew unbounded), the
// OLDEST entries are the ones evicted, and an evicted task's ack stores a
// ZERO identity into the ledger (the conservative "no identity → hash
// confirm" semantics).
func TestInFlightCache_BoundedAt4096_EvictedAckWritesZeroIdentity(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)

	// Pre-seed one task whose identity we will EVICT (stamped oldest).
	evicted := "task-evicted"
	e.recordInFlightIdentity(evicted, &uploader.UploadResult{
		Identity:         queue.FileIdentity{MtimeNs: 1, CtimeNs: 1, Dev: 1, Ino: 1, Size: 1},
		IdentityComplete: true,
	})
	// Age it far into the past so the oldest-first eviction picks it.
	e.inFlight.mu.Lock()
	ent := e.inFlight.m[evicted]
	ent.stampedAt = time.Now().Add(-24 * time.Hour)
	e.inFlight.m[evicted] = ent
	e.inFlight.mu.Unlock()

	// Fill to the cap with fresh entries.
	for i := 0; i < 4095; i++ {
		e.recordInFlightIdentity(
			"task-fresh-"+string(rune('a'+i%26))+time.Now().String()+string(rune(i)),
			&uploader.UploadResult{
				Identity:         queue.FileIdentity{MtimeNs: int64(i), CtimeNs: int64(i), Dev: 1, Ino: uint64(i), Size: int64(i)},
				IdentityComplete: true,
			})
	}

	// N more brand-new entries on top of the full cap → evictions happen.
	over := 50
	for i := 0; i < over; i++ {
		e.recordInFlightIdentity(
			"task-over-"+time.Now().String()+string(rune(i)),
			&uploader.UploadResult{
				Identity:         queue.FileIdentity{MtimeNs: int64(1e6 + i), CtimeNs: int64(1e6 + i), Dev: 1, Ino: uint64(1e6 + i), Size: 1},
				IdentityComplete: true,
			})
	}

	e.inFlight.mu.Lock()
	size := len(e.inFlight.m)
	_, evictedStillThere := e.inFlight.m[evicted]
	e.inFlight.mu.Unlock()
	require.LessOrEqual(t, size, 4096,
		"the in-flight cache must be truly bounded at 4096 (pre-fix: fresh entries grew it past the cap)")
	require.False(t, evictedStillThere,
		"the oldest entry must be the one evicted")

	// The evicted task's ledger path: the ack finds NO identity → zero
	// identity stored (queue.CompleteReportedIdentity with ok=false zeroes
	// it — assert through the executor's ack handler).
	// Seed a ledger row for the evicted task via a full report flow:
	task := newTask("rule", "/data/evicted.txt")
	task.ID = evicted
	require.NoError(t, q.Enqueue(task))
	if _, err := q.DequeuePending(1); err != nil {
		t.Fatal(err)
	}
	e.persistResult(ctx, task, &uploader.UploadResult{SHA256: "sha-evicted", SizeBytes: 1}, nil)
	require.NoError(t, e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: evicted, Success: true}))

	rec, err := q.GetProcessedIdentity(task.RuleID, task.LocalPath)
	require.NoError(t, err)
	require.Equal(t, "sha-evicted", rec.SHA256, "the sha itself is reported truthfully")
	require.False(t, rec.Ident.MtimeNs != 0 || rec.Ident.CtimeNs != 0 || rec.Ident.Dev != 0 || rec.Ident.Ino != 0,
		"an evicted task's ack must store a ZERO identity (undecidable → hash confirm next check)")
}
