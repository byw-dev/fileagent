// Package dirstore provides a lightweight in-memory store for pending
// directory-listing requests.
//
// Flow:
//  1. The REST handler calls Register to allocate a result channel keyed by
//     request_id, then sends the ListDirectoryCommand to the agent.
//  2. When the agent responds via the gRPC stream the grpcserver handler calls
//     Deliver, which pushes the result into the channel.
//  3. The REST handler reads from the channel (with a context deadline) and
//     returns the listing to the HTTP client.
//  4. If the context expires before Deliver is called, Cancel cleans up.
package dirstore

import "sync"

// DirEntry is a single remote filesystem entry.
type DirEntry struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	IsDir      bool    `json:"is_dir"`
	Size       *int64  `json:"size"`
	ModifiedAt *string `json:"modified_at"`
}

// Result is the outcome of a directory listing request delivered by the agent.
type Result struct {
	Entries []DirEntry
	// Error is non-empty when the agent reported a listing error.
	Error string
}

// Store maps pending request IDs to single-element result channels.
// It is safe for concurrent use from multiple goroutines.
type Store struct {
	m sync.Map // map[string]chan Result
}

// New returns an empty Store.
func New() *Store { return &Store{} }

// Register allocates a buffered channel for requestID and returns it.
// The caller MUST call Cancel if it gives up (e.g. on timeout) to prevent
// the channel from leaking.
func (s *Store) Register(requestID string) <-chan Result {
	ch := make(chan Result, 1)
	s.m.Store(requestID, ch)
	return ch
}

// Deliver sends result to the channel registered under requestID and removes
// the entry from the store.  No-op when requestID is not registered (e.g. the
// caller already timed out and called Cancel).
func (s *Store) Deliver(requestID string, result Result) {
	if v, ok := s.m.LoadAndDelete(requestID); ok {
		if ch, ok := v.(chan Result); ok {
			select {
			case ch <- result:
			default:
				// The waiter has already given up (cancelled or timed out) and
				// is no longer reading from the channel.  Drop the result.
			}
		}
	}
}

// Cancel removes the entry for requestID without delivering a result.
// It is a no-op when the entry has already been delivered or cancelled.
func (s *Store) Cancel(requestID string) {
	s.m.Delete(requestID)
}
