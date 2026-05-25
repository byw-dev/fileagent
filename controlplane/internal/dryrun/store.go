// Package dryrun provides a lightweight in-memory store for pending dry-run
// rule-test requests.
//
// Flow:
//  1. The REST handler calls Register to allocate a result channel keyed by a
//     temporary rule_id, then sends PushRuleCommand{dry_run:true} to the agent.
//  2. When the agent responds via the gRPC stream the grpcserver handler calls
//     Deliver, which pushes the result into the channel.
//  3. The REST handler reads from the channel (with a 30 s deadline) and
//     returns the file list to the HTTP client.
//  4. If the context expires before Deliver is called, Cancel cleans up.
package dryrun

import (
	"sync"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
)

// Store maps pending dry-run request IDs to single-element result channels.
// It is safe for concurrent use from multiple goroutines.
type Store struct {
	m sync.Map // map[string]chan *agentv1.DryRunResult
}

// New returns an empty Store.
func New() *Store { return &Store{} }

// Register allocates a buffered channel for reqID and returns it.
// The caller MUST call Cancel if it gives up (e.g. on timeout) to prevent
// the channel from leaking.
func (s *Store) Register(reqID string) <-chan *agentv1.DryRunResult {
	ch := make(chan *agentv1.DryRunResult, 1)
	s.m.Store(reqID, ch)
	return ch
}

// Deliver sends result to the channel registered under reqID and removes
// the entry from the store. No-op when reqID is not registered (e.g. the
// caller already timed out and called Cancel).
func (s *Store) Deliver(reqID string, result *agentv1.DryRunResult) {
	if v, ok := s.m.LoadAndDelete(reqID); ok {
		if ch, ok := v.(chan *agentv1.DryRunResult); ok {
			select {
			case ch <- result:
			default:
			}
		}
	}
}

// Cancel removes the entry for reqID without delivering a result.
// It is a no-op when the entry has already been delivered or cancelled.
func (s *Store) Cancel(reqID string) {
	s.m.Delete(reqID)
}
