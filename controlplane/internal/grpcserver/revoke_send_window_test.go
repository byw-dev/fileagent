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

// TestRevoke_Confirmed_DeliversCommandBeforeStreamEnd is the core IC-BUG-32
// acceptance: a NORMAL agent — one that is reading its stream, with no
// artificial backlog — must receive the RevokeCommand before the stream ends
// when the write was confirmed. The confirmed path ends the RPC gracefully
// (Stop, no cancel), so the ordering is guaranteed by the protocol teardown
// (queued DATA frames flush before the trailers), not by a race; this test is
// therefore deterministic and is gated on -count=50.
//
// Deliberately NOT a pinned-writer test: an end-to-end delivery assertion
// under a multi-megabyte unread backlog is unsound in this grpc-go version —
// the client transport can discard still-buffered DATA when the trailers
// arrive, independently of the control-plane mechanism (verified by byte-level
// capture on both directions: the server's wire order was correct while the
// client application saw 1 of 4 messages). The race that IC-BUG-32 describes
// is inherently probabilistic to reproduce end-to-end; its red evidence is
// recorded in the task report and the deterministic guards for the mechanism
// live in the handler-level mock tests.
func TestRevoke_Confirmed_DeliversCommandBeforeStreamEnd(t *testing.T) {
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
