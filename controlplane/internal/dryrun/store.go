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
	m sync.Map // map[string]*pending
}

// pending couples a waiting channel with the agent the request was sent to, so
// a result can be matched against its intended recipient.
type pending struct {
	ch      chan *agentv1.DryRunResult
	agentID string
}

// New returns an empty Store.
func New() *Store { return &Store{} }

// Register allocates a buffered channel for reqID, recording which agent the
// request is being sent to, and returns the channel.
// The caller MUST call Cancel if it gives up (e.g. on timeout) to prevent
// the channel from leaking.
func (s *Store) Register(reqID, agentID string) <-chan *agentv1.DryRunResult {
	ch := make(chan *agentv1.DryRunResult, 1)
	s.m.Store(reqID, &pending{ch: ch, agentID: agentID})
	return ch
}

// Deliver sends result to the channel registered under reqID, but only when
// agentID matches the agent the request was sent to. It removes the entry on a
// match, and is a no-op when reqID is unknown (e.g. the caller already timed out).
//
// The recipient check is the ownership rule for this path: reqID is a
// correlation id the Control Plane allocated for one specific agent, so
// "was this addressed to you" is the question worth asking. Checking it here
// rather than at the caller keeps the invariant next to the state it protects.
// A mismatch is reported so the caller can log it.
func (s *Store) Deliver(reqID, agentID string, result *agentv1.DryRunResult) bool {
	v, ok := s.m.Load(reqID)
	if !ok {
		return false
	}
	p, ok := v.(*pending)
	if !ok || p.agentID != agentID {
		return false
	}
	s.m.Delete(reqID)
	select {
	case p.ch <- result:
	default:
	}
	return true
}

// Cancel removes the entry for reqID without delivering a result.
// It is a no-op when the entry has already been delivered or cancelled.
func (s *Store) Cancel(reqID string) {
	s.m.Delete(reqID)
}
