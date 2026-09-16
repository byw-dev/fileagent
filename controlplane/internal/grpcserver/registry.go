package grpcserver

import (
	"context"
	"sync"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"google.golang.org/grpc"
)

// sendChCapacity is the buffer size of each connection's send channel. It is
// also the threshold past which a burst enqueued with no active consumer
// starts dropping silently — tests pin that the send goroutine (the consumer)
// is running before anything is enqueued (IC-BUG-31).
const sendChCapacity = 32

// AgentConn represents a single connected agent stream.
type AgentConn struct {
	AgentID     string
	Stream      grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.ServerMessage]
	SendCh      chan *agentv1.ServerMessage
	ConnectedAt time.Time
	CancelFunc  context.CancelFunc
	// SyncDegraded records whether THIS connection's rule sync ran degraded
	// (snapshot over the size budget, delivered as the keep-alive marker
	// instead). The Control Plane — not the cache — is the authority on the
	// degraded condition: the cache is a projection that can lose keys to
	// eviction or restart, and heartbeat renewal must rebuild from this state,
	// not from key existence (review R6). Written once during Connect setup
	// and read by the same connection's heartbeat handling — no concurrent
	// access.
	SyncDegraded bool

	// sendMu guards the write-confirmation state below (IC-BUG-32). Lock
	// order: the registry's r.mu.RLock covers only the map lookup and is
	// always released (or held only across a non-blocking enqueue) BEFORE any
	// waiting; the consumer goroutine never takes r.mu. The enqueue under
	// sendMu must stay non-blocking (select + default): blocking on a full
	// SendCh while holding sendMu would deadlock against the consumer, which
	// needs sendMu to publish write progress.
	sendMu sync.Mutex
	// queuedSeq is the sequence number assigned to the most recent enqueue.
	// Every channel element consumes one sequence number at enqueue time —
	// Send and SendSync alike — so that FIFO order equals sequence order and a
	// waiter can tell when its own message has been written.
	queuedSeq uint64
	// sentSeq is the number of messages the send goroutine has successfully
	// written to the stream. With one consumer and FIFO delivery it is also
	// the sequence number of the last written message.
	sentSeq uint64
	// notify is the generation channel for write progress: it is closed (and
	// replaced) every time sentSeq advances, so waiters can select on it.
	notify chan struct{}
	// writerDone is closed exactly once when the send goroutine stops for any
	// reason (ctx done, SendCh closed, send error). Waiters select on it so a
	// torn-down connection releases them immediately instead of burning the
	// timeout (IC-BUG-28: a dismantled connection must never leave a waiter
	// hanging).
	writerDone chan struct{}
	// registry is the back-reference sendSync needs to take r.mu for the
	// enqueue (set at Register time; the conn is only ever used through its
	// owning registry).
	registry *AgentRegistry
	// stopCh, closed once by Stop, asks Connect to end the RPC gracefully:
	// the handler returns without cancelling the stream context, so gRPC
	// flushes the queued DATA frames before the trailers (IC-BUG-32). It is
	// the confirmed-revoke path's release; CancelFunc is the forced one.
	stopCh   chan struct{}
	stopOnce sync.Once
}

// requestStop signals the graceful end. Idempotent: calling it more than once
// (e.g. a double revoke inside the teardown window) must not panic.
func (c *AgentConn) requestStop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// isStopping reports whether a graceful stop has been requested.
func (c *AgentConn) isStopping() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// AgentRegistry tracks all active bidirectional agent connections.
type AgentRegistry struct {
	mu    sync.RWMutex
	conns map[string]*AgentConn
}

// NewAgentRegistry creates an empty AgentRegistry.
func NewAgentRegistry() *AgentRegistry {
	return &AgentRegistry{
		conns: make(map[string]*AgentConn),
	}
}

// Register adds the agent connection and returns the AgentConn.
func (r *AgentRegistry) Register(
	agentID string,
	stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.ServerMessage],
	cancelFn context.CancelFunc,
) *AgentConn {
	conn := &AgentConn{
		AgentID:     agentID,
		Stream:      stream,
		SendCh:      make(chan *agentv1.ServerMessage, sendChCapacity),
		ConnectedAt: time.Now(),
		CancelFunc:  cancelFn,
		notify:      make(chan struct{}),
		writerDone:  make(chan struct{}),
		stopCh:      make(chan struct{}),
		registry:    r,
	}
	r.mu.Lock()
	if prev, ok := r.conns[agentID]; ok && prev != conn && prev.CancelFunc != nil {
		// A reconnect displaced the previous connection. Cancel it here: its
		// handler returns, its deferred Unregister no-ops on the identity
		// check, and no stale stream can outlive its registry entry to keep
		// heartbeating or uploading after a revoke that raced the reconnect
		// (PR #109 review P1-3). Same family as IC-BUG-28's identity rule —
		// registry entries and their teardown are per CONNECTION, not per id.
		prev.CancelFunc()
	}
	r.conns[agentID] = conn
	r.mu.Unlock()
	return conn
}

// Unregister removes only the current connection and reports whether it owned
// the registry entry. Send holds the same lock until its nonblocking send ends.
func (r *AgentRegistry) Unregister(conn *AgentConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if conn == nil || r.conns[conn.AgentID] != conn {
		return false
	}
	close(conn.SendCh)
	delete(r.conns, conn.AgentID)
	return true
}

// Disconnect tears down the agent's stream by cancelling the context Connect
// derived it from. It reports whether a connection was actually cancelled.
//
// Revocation used to be purely cooperative: the Control Plane sent a Revoke
// command and relied on the agent deleting its own token. A compromised agent
// simply ignores it, and nothing else stopped it — it kept heartbeating (so the
// UI showed it online with no way to kick it), kept reporting uploads and kept
// answering directory listings. See IC-BUG-25.
//
// The connection removes itself from the registry via Connect's deferred
// Unregister, so this only cancels; it must not close SendCh itself, or that
// deferred Unregister would close an already-closed channel.
func (r *AgentRegistry) Disconnect(agentID string) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	r.mu.RUnlock()
	if !ok || conn.CancelFunc == nil {
		return false
	}
	conn.CancelFunc()
	return true
}

// RevokeConn performs the whole revoke teardown bound to ONE connection: it
// captures the agent's current connection, enqueues msg on it, waits the
// bounded timeout for the write to be confirmed, then ends THAT connection —
// graceful (stop, normal RPC end) on confirmation, forced (cancel) on timeout.
// It reports whether the teardown was graceful.
//
// The binding matters (PR #109 review P1-3): a reconnect can register a new
// connection under the same id while the wait is in flight, and a by-id
// teardown would then cut the replacement while the stale connection — the
// one the command was confirmed on — lives on outside the registry, free to
// keep heartbeating and uploading (nothing on an established stream re-checks
// the DB revocation state). Register cancels a connection it displaces, so
// both ends of that race are covered; this is the same family as IC-BUG-28's
// identity rule: teardown is by connection, never by id.
func (r *AgentRegistry) RevokeConn(agentID string, msg *agentv1.ServerMessage, timeout time.Duration) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	if conn.sendSync(msg, timeout) {
		r.stopConn(conn)
		return true
	}
	r.disconnectConn(conn)
	return false
}

// stopConn asks one specific connection to end gracefully.
func (r *AgentRegistry) stopConn(conn *AgentConn) bool {
	if conn == nil {
		return false
	}
	conn.requestStop()
	return true
}

// disconnectConn force-cancels one specific connection's context.
func (r *AgentRegistry) disconnectConn(conn *AgentConn) bool {
	if conn == nil || conn.CancelFunc == nil {
		return false
	}
	conn.CancelFunc()
	return true
}

// Get returns the AgentConn for the given agent ID, or nil if not connected.
func (r *AgentRegistry) Get(agentID string) *AgentConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[agentID]
}

// Stop asks the agent's connection to end gracefully: the Connect handler
// returns without cancelling the stream context, so gRPC finishes the RPC
// normally — the transport's FIFO flushes the queued DATA frames before the
// trailers, and a command confirmed written (SendSync) deterministically
// reaches the agent before the stream ends. Contrast Disconnect, which
// cancels the context and can discard queued frames with the RST.
//
// Stop itself never waits: it only closes a channel and returns, so no caller
// can be dragged into waiting on a non-reading agent. It reports whether a
// connection was asked to stop, and is safe to call more than once.
func (r *AgentRegistry) Stop(agentID string) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	conn.requestStop()
	return true
}

// enqueue performs the non-blocking send and assigns the FIFO sequence number
// the caller can wait on. The caller must hold r.mu (read) across this call:
// that is what serialises the send against Unregister's close(SendCh).
//
// The send MUST be non-blocking (select + default). With a full SendCh,
// blocking here while holding sendMu would deadlock the consumer, which needs
// sendMu to publish write progress (see sendMu).
func (c *AgentConn) enqueue(msg *agentv1.ServerMessage) (uint64, bool) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	select {
	case c.SendCh <- msg:
		c.queuedSeq++
		return c.queuedSeq, true
	default:
		return 0, false
	}
}

// noteWritten is called by the send goroutine after a successful stream.Send.
// It advances the confirmed sequence and wakes every waiter.
func (c *AgentConn) noteWritten() {
	c.sendMu.Lock()
	c.sentSeq++
	close(c.notify)
	c.notify = make(chan struct{})
	c.sendMu.Unlock()
}

// markWriterStopped releases every pending write-confirmation waiter when the
// send goroutine stops for any reason. Called exactly once, deferred at the
// goroutine's single exit path.
func (c *AgentConn) markWriterStopped() {
	close(c.writerDone)
}

// sendSync is SendSync for one specific connection (no registry lookup), so a
// caller that captured a conn keeps talking to THAT conn even if a reconnect
// replaces the registry entry mid-flight (PR #109 review P1-3).
func (c *AgentConn) sendSync(msg *agentv1.ServerMessage, timeout time.Duration) bool {
	r := c.registry
	if r == nil {
		return false
	}
	// The enqueue runs under the registry read lock, and the connection's
	// identity is re-verified in the SAME critical section. Either alone is
	// not enough: holding the RLock only fences a CONCURRENT Unregister — an
	// Unregister that already COMPLETED between the caller's capture and this
	// acquisition has closed SendCh for good, and re-acquiring the read lock
	// will not reopen it (PR #109 re-review P1-a, process-panic DoS). The
	// identity check closes that window: if the entry is gone or was replaced,
	// the enqueue is refused and the caller falls back to the forced teardown
	// of its captured connection.
	r.mu.RLock()
	if r.conns[c.AgentID] != c {
		r.mu.RUnlock()
		return false
	}
	seq, enqueued := c.enqueue(msg)
	r.mu.RUnlock()
	if !enqueued {
		return false
	}
	return c.waitForWrite(seq, timeout)
}

// waitForWrite blocks a bounded time until the message enqueued as seq has
// been written to the stream (sentSeq >= seq), the send goroutine stops, or
// the timeout elapses. It holds NO registry lock and no sendMu while waiting.
func (c *AgentConn) waitForWrite(seq uint64, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.sendMu.Lock()
		if c.sentSeq >= seq {
			c.sendMu.Unlock()
			return true
		}
		notify := c.notify
		done := c.writerDone
		c.sendMu.Unlock()
		select {
		case <-notify:
			// Write progress: re-check the sequence.
		case <-done:
			// The send goroutine stopped; nothing else will be written.
			return false
		case <-timer.C:
			return false
		}
	}
}

// Send enqueues a message to the agent's send channel.
// Returns true if the message was queued, false if the agent is not connected
// or the channel is full.
func (r *AgentRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	if !ok {
		r.mu.RUnlock()
		return false
	}
	_, sent := conn.enqueue(msg)
	r.mu.RUnlock()
	return sent
}

// SendSync enqueues a message and waits a bounded time for the send goroutine
// to confirm the message was written to the stream, returning true only then.
// It is the instrument for the one place where a lost command has a name
// (Revoke, IC-BUG-32): Send alone only proves the command entered the queue,
// and a Disconnect immediately after can cancel the stream before a send
// goroutine that is busy with an earlier message ever reaches it.
//
// The bound is the caller's responsibility and must stay hard: an agent that
// never reads its stream pins stream.Send in flow control forever, and this
// call must never be the thing that waits for it. The enqueue is non-blocking
// (a full queue is a refusal, as with Send), the wait holds no locks, and a
// connection torn down mid-wait releases the waiter immediately via
// writerDone. The confirmation means "written to the transport", not "read by
// the agent" — flow control guarantees the former is as far as an honest
// bounded wait can go.
//
// Scope of the guarantee, stated plainly (IC-BUG-32): a CONFIRMED send pairs
// with a graceful end (Stop), whose teardown order deterministically delivers
// the queued message before the stream ends. An UNCONFIRMED send — the
// timeout expired — keeps the pre-fix best-effort behaviour: the caller
// falls back to a forceful cancel, which can still lose the command. The cap
// bounds that branch; it does not fix it.
func (r *AgentRegistry) SendSync(agentID string, msg *agentv1.ServerMessage, timeout time.Duration) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return conn.sendSync(msg, timeout)
}

// IsOnline reports whether the agent currently has an active connection.
func (r *AgentRegistry) IsOnline(agentID string) bool {
	r.mu.RLock()
	_, ok := r.conns[agentID]
	r.mu.RUnlock()
	return ok
}
