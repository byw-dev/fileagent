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
	}
	r.mu.Lock()
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

// Get returns the AgentConn for the given agent ID, or nil if not connected.
func (r *AgentRegistry) Get(agentID string) *AgentConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[agentID]
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
func (r *AgentRegistry) SendSync(agentID string, msg *agentv1.ServerMessage, timeout time.Duration) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	if !ok {
		r.mu.RUnlock()
		return false
	}
	seq, enqueued := conn.enqueue(msg)
	r.mu.RUnlock()
	if !enqueued {
		return false
	}
	return conn.waitForWrite(seq, timeout)
}

// IsOnline reports whether the agent currently has an active connection.
func (r *AgentRegistry) IsOnline(agentID string) bool {
	r.mu.RLock()
	_, ok := r.conns[agentID]
	r.mu.RUnlock()
	return ok
}
