package grpcserver

import (
	"context"
	"testing"
	"time"

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

// ── SendSync (IC-SEC-2 ③) ─────────────────────────────────────────────────────

// startFakeConsumer mimics the production send goroutine at registry level:
// it consumes SendCh in order and publishes write progress per message.
func startFakeConsumer(conn *AgentConn) (stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer conn.markWriterStopped()
		for msg := range conn.SendCh {
			_ = msg
			conn.noteWritten()
		}
	}()
	return func() { close(conn.SendCh); <-done }
}

func TestSendSync_ConfirmsAfterWrite(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	stop := startFakeConsumer(conn)
	defer stop()

	msg := &agentv1.ServerMessage{Payload: &agentv1.ServerMessage_Ping{Ping: &agentv1.PingCommand{}}}
	assert.True(t, r.SendSync("agent-1", msg, time.Second),
		"the consumer confirmed the write, so SendSync must report success")
}

func TestSendSync_TimeoutWhenNothingIsWritten(t *testing.T) {
	r := NewAgentRegistry()
	_ = r.Register("agent-1", nil, func() {})
	// No consumer: nothing is ever dequeued, nothing is ever written.

	start := time.Now()
	ok := r.SendSync("agent-1", &agentv1.ServerMessage{}, 100*time.Millisecond)
	elapsed := time.Since(start)
	assert.False(t, ok, "a write that never happened must not be confirmed")
	assert.Less(t, elapsed, 500*time.Millisecond,
		"the bound is hard: the waiter must not hang past it")
}

func TestSendSync_AgentNotConnected(t *testing.T) {
	r := NewAgentRegistry()
	assert.False(t, r.SendSync("nobody", &agentv1.ServerMessage{}, 100*time.Millisecond))
}

func TestSendSync_QueueFull_IsRefusal(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	defer func() { require.True(t, r.Unregister(conn)) }()
	// No consumer: the buffer fills up and stays full.
	for i := 0; i < sendChCapacity; i++ {
		require.True(t, r.Send("agent-1", &agentv1.ServerMessage{}))
	}
	assert.False(t, r.SendSync("agent-1", &agentv1.ServerMessage{}, time.Second),
		"a full queue is a refusal, exactly as with Send — never a blocking wait")
}

// TestSendSync_InterleavedSends confirms that messages enqueued with plain
// Send consume sequence numbers too: a SendSync waiter must not be released
// by writes of messages that were queued behind its own.
func TestSendSync_InterleavedSends(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	stop := startFakeConsumer(conn)
	defer stop()

	msg := &agentv1.ServerMessage{Payload: &agentv1.ServerMessage_Ping{Ping: &agentv1.PingCommand{}}}
	require.True(t, r.SendSync("agent-1", msg, time.Second))
	// Repeat a few rounds: sequence assignment must stay exact under reuse.
	for i := 0; i < 10; i++ {
		assert.True(t, r.Send("agent-1", msg))
		assert.True(t, r.SendSync("agent-1", msg, time.Second))
	}
}

// ── Stop (IC-SEC-2 ③ graceful path) ───────────────────────────────────────────

// Stop marks the connection stopping, is idempotent, and is orthogonal to
// Disconnect: graceful and forced teardown must not get in each other's way.
func TestStop_MarksStopping_Idempotent(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	defer func() { require.True(t, r.Unregister(conn)) }()

	assert.False(t, conn.isStopping(), "a fresh connection is not stopping")
	assert.True(t, r.Stop("agent-1"))
	assert.True(t, conn.isStopping())
	// Second stop must not panic (close of closed channel) — double revokes
	// inside the teardown window are real.
	assert.True(t, r.Stop("agent-1"))
	assert.False(t, r.Stop("nobody"))

	// Stop never touches the cancel func: Disconnect keeps its own semantics.
	cancelled := make(chan struct{})
	conn2 := r.Register("agent-2", nil, func() { close(cancelled) })
	defer func() { require.True(t, r.Unregister(conn2)) }()
	r.Stop("agent-2")
	select {
	case <-cancelled:
		t.Fatal("Stop must not cancel the stream context — that is Disconnect's job")
	default:
	}
	require.True(t, r.Disconnect("agent-2"))
	select {
	case <-cancelled:
		// correct: Disconnect is the one that cancels.
	default:
		t.Fatal("Disconnect must cancel the stream context")
	}
}
