package grpcserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// stubAgentMgr satisfies handler.AgentManager for the REST revoke path.
type stubAgentMgr struct{}

func (stubAgentMgr) ApproveAgent(_ context.Context, _ uuid.UUID, _ uuid.UUID) (string, error) {
	return "", nil
}
func (stubAgentMgr) RevokeAgent(_ context.Context, _ uuid.UUID, _ uuid.UUID) error { return nil }

// bigPayloadSize makes each queued message far larger than the stream's
// flow-control window, so a handful of them exhaust the quota as long as the
// client refuses to read. It stays under the default 4 MiB receive limit.
const bigPayloadSize = 100 << 10

// pinProbe is how long the pin loop waits for the send goroutine to consume a
// freshly enqueued message. A free goroutine dequeues in microseconds; this
// is four orders of magnitude of margin, so "no drain within the probe" is
// positive evidence of a blocked goroutine, not a scheduling hiccup.
const pinProbe = 50 * time.Millisecond

// pinSendGoroutine enqueues oversized messages while the client is not
// reading, until the queue demonstrably stops draining. Every unread byte
// holds flow-control quota, so once the queue no longer drains the send
// goroutine can only be inside a blocked stream.Send — this is the asserted
// precondition of the IC-BUG-32 race test, not something taken on faith.
// It returns the queue depth at pin time; the caller needs one slot free for
// the RevokeCommand itself.
func pinSendGoroutine(t *testing.T, registry *AgentRegistry, agentID string) int {
	t.Helper()
	big := func() *agentv1.ServerMessage {
		return &agentv1.ServerMessage{Payload: &agentv1.ServerMessage_PushRule{PushRule: &agentv1.PushRuleCommand{
			Rule: &agentv1.CollectionRule{RuleId: "big", BasePath: strings.Repeat("x", bigPayloadSize)},
		}}}
	}
	depth := len(registry.Get(agentID).SendCh)
	for i := 0; i < sendChCapacity-2; i++ {
		require.True(t, registry.Send(agentID, big()), "queue must have room while pinning")
		depth++
		drained := false
		deadline := time.Now().Add(pinProbe)
		for time.Now().Before(deadline) {
			if len(registry.Get(agentID).SendCh) < depth {
				drained = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !drained {
			// The goroutine stopped consuming with messages still queued:
			// it is provably pinned inside stream.Send.
			return depth
		}
		depth = len(registry.Get(agentID).SendCh)
	}
	t.Fatal("could not pin the send goroutine: the flow-control window swallowed the whole queue")
	return 0
}

// TestRevoke_IdlePath_DeliversCommandBeforeStreamEnd guards the ONLY half of
// IC-BUG-32 that is deterministic server-side: with an IDLE send path (no
// backlog, the writer parked on select) the queued command is handed to the
// transport and flushed before any teardown, so the reading agent receives it
// before the stream ends. This is a contract guard for the idle path — it is
// NOT fix-evidence for the backlog race (that path delivered ~100% before this
// knife as well; see the card's 20000/20000 measurement) and it must never be
// cited as proof that the pinned-scenario flake is eliminated.
//
// The pinned/backlogged scenario has its own test below with honest,
// weaker assertions — its delivery outcome cannot be asserted from the
// server side at all.
func TestRevoke_IdlePath_DeliversCommandBeforeStreamEnd(t *testing.T) {
	agentID := "44444444-4444-4444-4444-444444444444"
	client, bearer, registry := newFullServerForAgent(t, agentID, &mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "agent must be registered")

	// The client reads from the start — a normal, cooperative agent.
	received := make(chan *agentv1.ServerMessage, 16)
	streamErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				streamErr <- err
				return
			}
			received <- msg
		}
	}()

	h := handler.NewAgentsHandler(nil, stubAgentMgr{}, nil, registry, zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		claims := &auth.Claims{}
		claims.Role = "super_admin"
		claims.OrgID = uuid.New().String()
		claims.Subject = uuid.New().String()
		c.Set("jwt_claims", claims)
		c.Next()
	})
	r.POST("/api/v1/agents/:id/revoke", h.Revoke)

	start := time.Now()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
	r.ServeHTTP(w, req)
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Less(t, elapsed, 3*time.Second,
		"revoke must respect the hard cap plus margin")

	// The queued RevokeCommand must reach the client before the stream ends.
	var gotRevoke bool
	for !gotRevoke {
		select {
		case msg := <-received:
			if msg.GetRevoke() != nil {
				gotRevoke = true
			}
		case err := <-streamErr:
			t.Fatalf("stream ended (%v) before RevokeCommand was delivered", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for RevokeCommand / stream end")
		}
	}

	// …and the stream must then be torn down.
	select {
	case err := <-streamErr:
		assert.Equal(t, codes.PermissionDenied, status.Code(err),
			"the stream must be cut after the command was delivered")
	case <-time.After(3 * time.Second):
		t.Fatal("stream was not torn down after revoke")
	}
	require.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		2*time.Second, 10*time.Millisecond, "handler must return and unregister")
}

// TestRevoke_Backlog_HardCapAndCut restores the pinned-scenario coverage of
// IC-BUG-32 with assertions the server can actually keep.
//
// The setup is the original falsifiable one (kept verbatim from the first
// cut): the send goroutine is pinned inside stream.Send behind an unread
// backlog, the RevokeCommand is enqueued behind it, and only then does the
// client start reading. What is asserted:
//   - the pin precondition itself (the reproduction is real, not assumed);
//   - the revoke returns within the hard cap plus margin;
//   - the stream IS cut and the registry entry reclaimed.
//
// What is deliberately NOT asserted: that the command arrives before the cut.
// It cannot be, from the server side: stream.Send returns when the frame is
// queued, not flushed, there is no API to wait for the wire, and the stream
// teardown does not wait for the peer to open its flow-control window —
// unflushed queued frames are discarded at teardown. Measured on this exact
// harness: ~50% delivery with the pre-fix Send+Disconnect ordering and ~45%
// with the graceful ordering (byte-level capture: the server's wire order was
// correct; the client transport received every byte; the application saw 1 of
// 4 messages). Delivery under backlog is therefore best-effort and is
// recorded as the unclosed half of IC-BUG-32, not silently asserted away.
// The deterministic idle-path contract has its own test above.
func TestRevoke_Backlog_HardCapAndCut(t *testing.T) {
	agentID := "44444444-4444-4444-4444-444444444444"
	client, bearer, registry := newFullServerForAgent(t, agentID, &mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "agent must be registered")

	// ── Precondition: pin the send goroutine inside stream.Send ──────────────
	// The client is NOT reading yet; pinSendGoroutine asserts the pin.
	depth := pinSendGoroutine(t, registry, agentID)

	// ── Fire the REST revoke; gate on the command being enqueued ─────────────
	h := handler.NewAgentsHandler(nil, stubAgentMgr{}, nil, registry, zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		claims := &auth.Claims{}
		claims.Role = "super_admin"
		claims.OrgID = uuid.New().String()
		claims.Subject = uuid.New().String()
		c.Set("jwt_claims", claims)
		c.Next()
	})
	r.POST("/api/v1/agents/:id/revoke", h.Revoke)

	revokeStarted := time.Now()
	revokeDone := make(chan struct{})
	revokeCode := http.StatusInternalServerError
	go func() {
		defer close(revokeDone)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
		r.ServeHTTP(w, req)
		revokeCode = w.Code
	}()
	require.Eventually(t, func() bool {
		conn := registry.Get(agentID)
		return conn == nil || len(conn.SendCh) == depth+1
	}, 2*time.Second, 5*time.Millisecond, "the revoke command must be enqueued")

	// ── Only now does the client start reading ───────────────────────────────
	received := make(chan *agentv1.ServerMessage, 16)
	streamErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				streamErr <- err
				return
			}
			received <- msg
		}
	}()

	select {
	case <-revokeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke handler did not return within the hard cap + margin")
	}
	revokeElapsed := time.Since(revokeStarted)

	// The stream must be torn down — the one promise that holds for every
	// backlog shape. Whether the command itself made it out is best-effort
	// (see the doc comment) and is only logged, never asserted.
	var gotRevoke bool
delivered:
	for {
		select {
		case msg := <-received:
			if msg.GetRevoke() != nil {
				gotRevoke = true
			}
		case err := <-streamErr:
			assert.Equal(t, codes.PermissionDenied, status.Code(err),
				"the stream must be cut after the revoke")
			break delivered
		case <-time.After(3 * time.Second):
			t.Fatal("stream was not torn down after revoke")
		}
	}
	t.Logf("backlog revoke: delivered_before_cut=%v (best-effort, not asserted)", gotRevoke)

	require.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		2*time.Second, 10*time.Millisecond, "handler must return and unregister")
	assert.Equal(t, http.StatusOK, revokeCode)
	assert.Less(t, revokeElapsed, 3*time.Second,
		"revoke must respect the hard cap plus margin")
}

// TestSendSync_WaiterReleasedWhenWriterStops parks a SendSync waiter behind a
// pinned send, tears the stream down (Disconnect), and requires the waiter to
// be released promptly. The release path is the production send goroutine's
// deferred markWriterStopped on exit; if that ever disappears, the waiter
// burns its whole timer instead — the IC-BUG-28 invariant that a dismantled
// connection must never leave a waiter hanging, pinned here on purpose.
func TestSendSync_WaiterReleasedWhenWriterStops(t *testing.T) {
	agentID := "55555555-5555-5555-5555-555555555555"
	client, bearer, registry := newFullServerForAgent(t, agentID, &mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "agent must be registered")

	// Client never reads: the send goroutine pins itself in stream.Send.
	depth := pinSendGoroutine(t, registry, agentID)

	waiterCh := make(chan bool, 1)
	go func() {
		waiterCh <- registry.SendSync(agentID, &agentv1.ServerMessage{}, 5*time.Second)
	}()
	require.Eventually(t, func() bool {
		conn := registry.Get(agentID)
		return conn != nil && len(conn.SendCh) == depth+1
	}, 2*time.Second, 5*time.Millisecond, "waiter's message must be parked behind the pinned send")
	_ = stream

	start := time.Now()
	require.True(t, registry.Disconnect(agentID), "the stream must be cut")
	select {
	case ok := <-waiterCh:
		assert.False(t, ok, "a torn-down connection can never confirm the write")
		assert.Less(t, time.Since(start), 2*time.Second,
			"release must come from the writer stopping, not from the waiter's 5s timer")
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not released by teardown within 2s — the writer-stop release path is gone")
	}
}

// TestRevoke_NonReadingAgent_StillCutWithinCap is the hard-cap half of the
// IC-BUG-32 fix: the send goroutine is pinned inside a blocked stream.Send
// and the agent never reads, so the write-out confirmation can never arrive.
// The revoke must give up at the hard cap and cut the stream anyway — the
// handler returns within the cap plus margin, the client's stream ends, and
// the registry entry is reclaimed.
func TestRevoke_NonReadingAgent_StillCutWithinCap(t *testing.T) {
	agentID := "66666666-6666-6666-6666-666666666666"
	client, bearer, registry := newFullServerForAgent(t, agentID, &mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "agent must be registered")

	// The client never reads — not now, not after the revoke.
	depth := pinSendGoroutine(t, registry, agentID)

	h := handler.NewAgentsHandler(nil, stubAgentMgr{}, nil, registry, zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		claims := &auth.Claims{}
		claims.Role = "super_admin"
		claims.OrgID = uuid.New().String()
		claims.Subject = uuid.New().String()
		c.Set("jwt_claims", claims)
		c.Next()
	})
	r.POST("/api/v1/agents/:id/revoke", h.Revoke)

	start := time.Now()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
	r.ServeHTTP(w, req)
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Less(t, elapsed, 3*time.Second,
		"the hard cap is hard: the handler must not wait on a non-reading agent")

	// The stream is cut regardless: the client's Recv ends with
	// PermissionDenied, and the handler's teardown reclaims the entry.
	errCh := make(chan error, 1)
	go func() {
		for {
			_, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
		}
	}()
	select {
	case err := <-errCh:
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	case <-time.After(2 * time.Second):
		t.Fatal("stream was not cut within the cap + margin")
	}
	require.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		2*time.Second, 10*time.Millisecond, "handler must return and unregister")
	_ = depth
}

// TestRevoke_GracefulStop_ConnectGoroutinesReleased walks the graceful path
// end to end — connect, revoke with a confirmed write (no backlog, so the
// parked send goroutine writes the command immediately), normal teardown —
// and then requires every goroutine Connect started to have exited.
//
// This is the regression for the recv-goroutine release (4a): on the graceful
// path ctx is never cancelled and the main loop is gone, so the old delivery
// select (blocking send to recvCh, ctx.Done as the only escape) leaked the
// helper forever. The stack dump is deterministic evidence: a leaked helper
// parks forever inside a Connect closure, so the assertion eventually fails;
// the fixed path converges to zero as soon as the RPC ends.
func TestRevoke_GracefulStop_ConnectGoroutinesReleased(t *testing.T) {
	agentID := "77777777-7777-7777-7777-777777777777"
	client, bearer, registry := newFullServerForAgent(t, agentID, &mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "agent must be registered")

	// Revoke with an empty queue: the parked send goroutine dequeues the
	// command immediately and confirms the write, so the handler takes the
	// graceful Stop path.
	h := handler.NewAgentsHandler(nil, stubAgentMgr{}, nil, registry, zap.NewNop())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		claims := &auth.Claims{}
		claims.Role = "super_admin"
		claims.OrgID = uuid.New().String()
		claims.Subject = uuid.New().String()
		c.Set("jwt_claims", claims)
		c.Next()
	})
	r.POST("/api/v1/agents/:id/revoke", h.Revoke)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
	revokeDone := make(chan struct{})
	go func() {
		defer close(revokeDone)
		r.ServeHTTP(w, req)
	}()
	select {
	case <-revokeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("revoke handler did not return — the graceful stop path is broken")
	}
	require.Equal(t, http.StatusOK, w.Code)

	// The client must still receive the command and then the end of stream:
	// buffered messages come out before the final status.
	received := make(chan *agentv1.ServerMessage, 4)
	streamErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				streamErr <- err
				return
			}
			received <- msg
		}
	}()
	var gotRevoke bool
	for !gotRevoke {
		select {
		case msg := <-received:
			if msg.GetRevoke() != nil {
				gotRevoke = true
			}
		case err := <-streamErr:
			t.Fatalf("stream ended (%v) before RevokeCommand was delivered", err)
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for RevokeCommand / stream end")
		}
	}
	select {
	case err := <-streamErr:
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	case <-time.After(3 * time.Second):
		t.Fatal("stream was not torn down after the graceful revoke")
	}

	// Gate: no goroutine started by Connect may remain. The handler has
	// returned (registry entry reclaimed), so any survivor is a leak.
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		return !strings.Contains(string(buf[:n]), "grpcserver.(*Server).Connect.func")
	}, 2*time.Second, 20*time.Millisecond,
		"Connect's send and recv goroutines must both exit after a graceful stop")
}
