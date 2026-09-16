package grpcserver

import (
	"context"
	"sync"
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

// ── Connection-bound revoke teardown (PR #109 review P1-3) ───────────────────

// Register must cancel the connection it displaces: a stale stream must never
// outlive its registry entry, or it can keep heartbeating and uploading after
// a revoke that raced the reconnect. Same family as IC-BUG-28's identity rule.
func TestRegister_DisplacesCancelsPreviousConnection(t *testing.T) {
	r := NewAgentRegistry()
	var onceO sync.Once
	cancelledO := make(chan struct{})
	r.Register("agent-1", nil, func() { onceO.Do(func() { close(cancelledO) }) })

	var onceN sync.Once
	cancelledN := make(chan struct{})
	r.Register("agent-1", nil, func() { onceN.Do(func() { close(cancelledN) }) })

	select {
	case <-cancelledO:
		// correct: the displaced connection dies with its registry entry.
	default:
		t.Fatal("the displaced connection was not cancelled — it is stranded outside the registry")
	}
	select {
	case <-cancelledN:
		t.Fatal("the replacement connection must not be cancelled by its own registration")
	default:
	}
}

// RevokeConn is bound to the connection it captured: a reconnect registering a
// replacement mid-wait must not redirect the cut, and the displaced connection
// must not survive. Here the confirm never lands (no consumer, so the write is
// never confirmed within the bound) — the revoke must return within the bound
// with the forced outcome, the captured connection must be cancelled, and the
// replacement must be left to its own lifecycle (its gate re-checks DB status;
// killing it is the reconnect gate's job, not this teardown's).
func TestRevokeConn_BindsToTheCapturedConnection(t *testing.T) {
	r := NewAgentRegistry()
	var onceO sync.Once
	cancelledO := make(chan struct{})
	r.Register("agent-1", nil, func() { onceO.Do(func() { close(cancelledO) }) })

	done := make(chan bool, 1)
	go func() {
		done <- r.RevokeConn("agent-1", &agentv1.ServerMessage{}, 2*time.Second)
	}()
	time.Sleep(50 * time.Millisecond)
	var onceN sync.Once
	cancelledN := make(chan struct{})
	r.Register("agent-1", nil, func() { onceN.Do(func() { close(cancelledN) }) })

	select {
	case graceful := <-done:
		assert.False(t, graceful, "no consumer: the write cannot confirm within the bound")
	case <-time.After(3 * time.Second):
		t.Fatal("RevokeConn did not return within the hard cap plus margin")
	}
	select {
	case <-cancelledO:
		// correct: the captured connection is torn down (by the displacement
		// cancel, and the forced fallback is a no-op on top of it).
	default:
		t.Fatal("the captured connection was not torn down")
	}
	// The cut must have landed on the CAPTURED connection: a by-id teardown
	// here would cancel the replacement — the one connection the revoke has
	// no business touching — while claiming to have handled the stale one.
	select {
	case <-cancelledN:
		t.Fatal("the teardown hit the replacement connection instead of the captured one (by-id regression)")
	default:
	}
}

// RevokeConn with a confirming consumer ends the captured connection
// gracefully (stop, not cancel) — the idle-path contract.
func TestRevokeConn_GracefulOnConfirmedConnection(t *testing.T) {
	r := NewAgentRegistry()
	cancelled := make(chan struct{})
	conn := r.Register("agent-1", nil, func() { close(cancelled) })
	stop := startFakeConsumer(conn)
	defer stop()

	graceful := r.RevokeConn("agent-1", &agentv1.ServerMessage{}, time.Second)
	assert.True(t, graceful, "the write confirmed, so the teardown must be graceful")
	assert.True(t, conn.isStopping(), "the graceful end must be a stop, not a cancel")
	select {
	case <-cancelled:
		t.Fatal("a graceful stop must not cancel the stream context")
	default:
	}
}

// PR #109 review P2: a plain Send consumes a sequence number too. The consumer
// here is gated, so the test can tell "the waiter's OWN message was written"
// apart from "an earlier plain message was written". If plain sends stopped
// consuming sequence numbers, the waiter would be confirmed by the plain
// message's write while its own command is still queued — and the revoke
// would cut the stream before the command ever left the queue.
func TestSendSync_WaitsForItsOwnMessageNotPlainSends(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})

	// Gated consumer: writes one message only when the test releases it.
	release := make(chan struct{}, 4)
	written := make(chan string, 4)
	stop := make(chan struct{})
	go func() {
		defer conn.markWriterStopped()
		for {
			select {
			case <-stop:
				return
			case msg := <-conn.SendCh:
				<-release
				conn.noteWritten()
				written <- msg.GetMessageId()
			}
		}
	}()
	defer close(stop)

	plain := &agentv1.ServerMessage{MessageId: "plain"}
	require.True(t, r.Send("agent-1", plain), "the queue has room for the plain message")

	syncDone := make(chan bool, 1)
	go func() {
		syncDone <- r.SendSync("agent-1", &agentv1.ServerMessage{MessageId: "sync"}, 5*time.Second)
	}()

	// Let exactly ONE message be written (the plain one — FIFO).
	release <- struct{}{}
	var firstWritten string
	select {
	case firstWritten = <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer never wrote the first message")
	}
	require.Equal(t, "plain", firstWritten)

	// The waiter must STILL be blocked: its own message has not been written.
	select {
	case ok := <-syncDone:
		t.Fatalf("SendSync returned %v after only the plain send was written — plain sends are not consuming sequence numbers", ok)
	case <-time.After(100 * time.Millisecond):
	}

	// Now release the sync message's write: the waiter must confirm.
	release <- struct{}{}
	select {
	case ok := <-syncDone:
		assert.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("SendSync never confirmed its own message")
	}
}

// PR #109 re-review P1-a: a connection-scoped sendSync must refuse (not
// panic) when the connection was unregistered between the caller's capture
// and the enqueue. Capture-and-enqueue are two lock acquisitions; an
// Unregister can complete in between and close(SendCh) — sending on it takes
// the whole process down. Inside the lock the conn's identity is re-checked:
// holding the RLock only fences a CONCURRENT Unregister, it cannot reopen a
// channel that a completed one already closed.
func TestSendSync_AfterUnregister_IsRefusedNotPanic(t *testing.T) {
	r := NewAgentRegistry()
	conn := r.Register("agent-1", nil, func() {})
	require.True(t, r.Unregister(conn), "setup: the connection is unregistered and its SendCh closed")

	require.NotPanics(t, func() {
		assert.False(t, conn.sendSync(&agentv1.ServerMessage{}, 50*time.Millisecond),
			"an unregistered connection must refuse sends, never enqueue on its closed channel")
	})
}
