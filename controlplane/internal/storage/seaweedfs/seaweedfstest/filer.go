//go:build integration

// Package seaweedfstest provides scripted fakes for wiring the real adapter
// in integration tests that live OUTSIDE this tree (cmd/server may import it
// per the D-037 import boundary, Rule B). It exists only under the
// integration build tag so normal builds never ship it.
package seaweedfstest

import (
	"context"
	"sync"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	seaweedfs "github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
)

// ResponseSpec is one scripted SubscribeMetadataResponse.
type ResponseSpec struct {
	Directory string
	Name      string
	Heartbeat bool
	FileSize  uint64
	TsNs      int64
}

// ToResponse converts the spec to the wire type (Translate-compatible).
func (r ResponseSpec) ToResponse() *filerpb.SubscribeMetadataResponse {
	if r.Heartbeat {
		return &filerpb.SubscribeMetadataResponse{TsNs: r.TsNs}
	}
	return &filerpb.SubscribeMetadataResponse{
		Directory: r.Directory,
		TsNs:      r.TsNs,
		EventNotification: &filerpb.EventNotification{
			NewEntry: &filerpb.Entry{
				Name:       r.Name,
				Attributes: &filerpb.FuseAttributes{FileSize: r.FileSize},
			},
		},
	}
}

// ScriptedFiler replays the scripted responses on the FIRST subscription;
// later subscriptions park until their context is done (the test cancels to
// end Run).
type ScriptedFiler struct {
	mu        sync.Mutex
	resps     []ResponseSpec
	subscribe int
}

// NewScriptedFiler builds a filer from the response specs.
func NewScriptedFiler(resps []ResponseSpec) *ScriptedFiler {
	return &ScriptedFiler{resps: resps}
}

// SubscribeCount reports how many SubscribeLocalMetadata calls were made.
func (f *ScriptedFiler) SubscribeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribe
}

type scriptedStream struct {
	ctx   context.Context
	resps []ResponseSpec
	i     int
}

func (s *scriptedStream) Recv() (*filerpb.SubscribeMetadataResponse, error) {
	if s.i < len(s.resps) {
		r := s.resps[s.i]
		s.i++
		return r.ToResponse(), nil
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// SubscribeLocalMetadata implements the filer subscription.
func (f *ScriptedFiler) SubscribeLocalMetadata(ctx context.Context, in *filerpb.SubscribeMetadataRequest) (seaweedfs.SubStream, error) {
	f.mu.Lock()
	f.subscribe++
	n := f.subscribe
	f.mu.Unlock()
	if n == 1 {
		return &scriptedStream{ctx: ctx, resps: f.resps}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// ListEntries satisfies the client interface (unused by these tests).
func (f *ScriptedFiler) ListEntries(context.Context, *filerpb.ListEntriesRequest) (seaweedfs.ListStreamAlias, error) {
	return nil, nil
}

// RecordCursorStore records saved cursors (for assertions).
type RecordCursorStore struct {
	Saves []int64
}

func (s *RecordCursorStore) DeclaredCursor(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

func (s *RecordCursorStore) SaveCursor(_ context.Context, _ string, v int64) error {
	s.Saves = append(s.Saves, v)
	return nil
}

func (s *RecordCursorStore) RecordMark(context.Context, seaweedfs.MarkAlias) error { return nil }

func (s *RecordCursorStore) CountActiveEntries(context.Context, string) (int64, error) {
	return 0, nil
}

// FixedClock counts readings and returns a fixed time.
type FixedClock struct {
	Calls int
	T     time.Time
}

func (c *FixedClock) Now(context.Context) (time.Time, error) {
	c.Calls++
	return c.T, nil
}
