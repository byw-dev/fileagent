package grpcserver

import (
	"context"
	"testing"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentRegistry_RegisterAndGet(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	require.NotNil(t, conn)
	assert.Equal(t, "agent-1", conn.AgentID)
	assert.NotNil(t, conn.SendCh)

	got := r.Get("agent-1")
	require.NotNil(t, got)
	assert.Equal(t, "agent-1", got.AgentID)
}

func TestAgentRegistry_Unregister(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	assert.True(t, r.IsOnline("agent-1"))

	r.Unregister(conn)
	assert.False(t, r.IsOnline("agent-1"))
	assert.Nil(t, r.Get("agent-1"))
}

func TestAgentRegistry_IsOnline(t *testing.T) {
	r := NewAgentRegistry()
	assert.False(t, r.IsOnline("agent-99"))

	r.Register("agent-99", nil, func() {})
	assert.True(t, r.IsOnline("agent-99"))
}

func TestAgentRegistry_Send_AgentNotConnected(t *testing.T) {
	r := NewAgentRegistry()
	msg := &agentv1.ServerMessage{}
	ok := r.Send("nonexistent", msg)
	assert.False(t, ok)
}

func TestAgentRegistry_Send_AgentConnected(t *testing.T) {
	r := NewAgentRegistry()
	r.Register("agent-2", nil, func() {})

	msg := &agentv1.ServerMessage{}
	ok := r.Send("agent-2", msg)
	assert.True(t, ok)

	conn := r.Get("agent-2")
	require.NotNil(t, conn)
	select {
	case received := <-conn.SendCh:
		assert.Equal(t, msg, received)
	default:
		t.Fatal("expected message in SendCh")
	}
}

func TestAgentRegistry_Send_ChannelFull(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-3", nil, func() {})

	// Fill the channel buffer.
	for i := 0; i < cap(conn.SendCh); i++ {
		ok := r.Send("agent-3", &agentv1.ServerMessage{})
		assert.True(t, ok)
	}

	// Next send should fail (channel full).
	ok := r.Send("agent-3", &agentv1.ServerMessage{})
	assert.False(t, ok)
}

// Disconnect must cancel the stream context so Connect returns and its deferred
// Unregister runs. It must not close SendCh itself — that deferred Unregister
// does, and closing twice would panic.
func TestAgentRegistry_Disconnect_CancelsStreamContext(t *testing.T) {
	r := NewAgentRegistry()
	_, cancel := context.WithCancel(context.Background())
	cancelled := false
	conn := r.Register("agent-1", nil, func() { cancelled = true; cancel() })
	require.NotNil(t, conn)

	assert.True(t, r.Disconnect("agent-1"))
	assert.True(t, cancelled, "the stream context must be cancelled")

	// SendCh stays open: Connect's deferred Unregister owns closing it.
	select {
	case _, ok := <-conn.SendCh:
		assert.True(t, ok, "Disconnect must not close SendCh")
	default:
	}

	r.Unregister(conn)
	assert.False(t, r.Disconnect("agent-1"), "an absent agent reports false")
}

// TestAgentRegistryReconnectWhileSending proves old streams cannot close replacements.
func TestAgentRegistryReconnectWhileSending(t *testing.T) {
	r := NewAgentRegistry()
	current := r.Register("agent", nil, func() {})
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
				r.Send("agent", &agentv1.ServerMessage{})
			}
		}
	}()
	for i := 0; i < 10000; i++ {
		fresh := r.Register("agent", nil, func() {})
		require.False(t, r.Unregister(current))
		require.True(t, r.IsOnline("agent"))
		current = fresh
	}
	close(done)
	<-finished
	require.True(t, r.Unregister(current))
	require.False(t, r.Unregister(current))
	require.False(t, r.Unregister(nil))
}
