package grpcserver

import (
	"context"
	"sync"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"google.golang.org/grpc"
)

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
		SendCh:      make(chan *agentv1.ServerMessage, 32),
		ConnectedAt: time.Now(),
		CancelFunc:  cancelFn,
	}
	r.mu.Lock()
	r.conns[agentID] = conn
	r.mu.Unlock()
	return conn
}

// Unregister removes the agent connection.
func (r *AgentRegistry) Unregister(agentID string) {
	r.mu.Lock()
	if conn, ok := r.conns[agentID]; ok {
		close(conn.SendCh)
		delete(r.conns, agentID)
	}
	r.mu.Unlock()
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
	r.mu.RUnlock()
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
