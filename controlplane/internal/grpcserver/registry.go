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

// Send enqueues a message to the agent's send channel.
// Returns true if the message was queued, false if the agent is not connected
// or the channel is full.
func (r *AgentRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	r.mu.RLock()
	conn, ok := r.conns[agentID]
	defer r.mu.RUnlock()
	if !ok {
		return false
	}
	select {
	case conn.SendCh <- msg:
		return true
	default:
		return false
	}
}

// IsOnline reports whether the agent currently has an active connection.
func (r *AgentRegistry) IsOnline(agentID string) bool {
	r.mu.RLock()
	_, ok := r.conns[agentID]
	r.mu.RUnlock()
	return ok
}
