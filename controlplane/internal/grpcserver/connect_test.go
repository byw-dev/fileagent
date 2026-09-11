package grpcserver

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newFullServer creates a test gRPC server with all deps wired (registry,
// mock cache, mock NATS, real JWT service backed by miniredis).
// Returns the client and the bearer token to use.
func newFullServer(t *testing.T) (agentv1.AgentServiceClient, string) {
	t.Helper()

	// Start miniredis for JWT revocation checks.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	// Create the JWT service and mint a token for "agent-test".
	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(
		"agent-test", "org-1", "agent", "agent-test", time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	c := newMockCache()

	srv := New(logger)
	srv.WithDeps(registry, c, jwtSvc, &mockNATS{}, nil)

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn), "Bearer " + token
}

// A revoked agent must not be able to open a stream. Connect is the entry point
// that pushes credentials automatically, so leaving it ungated while
// RefreshCredentials is gated would keep the whole attack chain intact: connect
// once, receive credentials, done.
//
// The extra assertions are deliberate. A status-code-only test would still pass
// if the gate were moved further down Connect, where the agent would already
// have been registered, marked online and handed credentials before being
// refused. IsOnline alone is not enough either — the deferred Unregister resets
// it — so the online write is what actually pins the gate's position.
func TestServer_Connect_RevokedAgent_IsRejectedBeforeRegistration(t *testing.T) {
	agentID := "22222222-2222-2222-2222-222222222222"
	stateDB := &mockStateDB{agentStatus: db.AgentStatusRevoked}
	client, bearer, registry := newFullServerForAgent(t, agentID, stateDB)

	// Bounded: if the gate is ever removed the server accepts the stream and
	// Recv would block forever, turning a regression into a hung suite instead
	// of a failing test.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err, "the stream opens; the rejection arrives on first Recv")

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err),
		"a stream that merely times out means the gate is gone")
	assert.False(t, registry.IsOnline(agentID),
		"a rejected agent must never enter the registry")
	assert.Zero(t, stateDB.markOnlineUsableCalls,
		"the gate must run before the online transition, not after it")
}

func TestServer_Connect_ApprovedAgent_IsAccepted(t *testing.T) {
	agentID := "33333333-3333-3333-3333-333333333333"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "m1"}))

	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "an approved agent must be registered")
}

// newFullServerForAgent is newFullServer with a caller-chosen agent id and state
// DB, so the liveness gate can be exercised.
func newFullServerForAgent(t *testing.T, agentID string, stateDB AgentStateDB) (agentv1.AgentServiceClient, string, *AgentRegistry) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(agentID, "org-1", "agent", agentID, time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	srv := New(logger)
	srv.WithDeps(registry, newMockCache(), jwtSvc, &mockNATS{}, nil)
	srv.WithStateDB(stateDB)

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn), "Bearer " + token, registry
}

func TestServer_Connect_WithRegistry_ReceivesAndDisconnects(t *testing.T) {
	client, bearerToken := newFullServer(t)

	md := metadata.Pairs("authorization", bearerToken)
	ctx, cancel := context.WithTimeout(
		metadata.NewOutgoingContext(context.Background(), md),
		3*time.Second,
	)
	defer cancel()

	stream, err := client.Connect(ctx)
	require.NoError(t, err)

	// Send a heartbeat to the server.
	err = stream.Send(&agentv1.AgentMessage{
		MessageId: "hb-1",
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{UptimeSeconds: 10},
		},
	})
	require.NoError(t, err)

	// Close the send side so the server exits the receive loop.
	require.NoError(t, stream.CloseSend())

	// After client closes, the server Recv will return EOF which causes Connect
	// to return. The client will get EOF or a status error.
	_, recvErr := stream.Recv()
	// We expect EOF or a stream-closed error (not an application-level failure).
	if recvErr != nil {
		code := status.Code(recvErr)
		assert.NotEqual(t, codes.Unauthenticated, code)
		assert.NotEqual(t, codes.Internal, code)
	}
}

func TestServer_Connect_WithRegistry_Unauthenticated_NoToken(t *testing.T) {
	client, _ := newFullServer(t)

	// No authorization header — JWT interceptor should reject.
	stream, err := client.Connect(context.Background())
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// Revocation has to actually end the RPC, not merely cancel a context nobody
// observes. The first attempt at IC-BUG-25 cancelled a context derived from
// stream.Context(), which the blocking Recv never looks at: the handler kept
// running, its registry entry stayed, and the agent went on sending.
//
// The assertion is therefore about the stream ending and the registry emptying,
// not about Disconnect returning true.
func TestServer_Connect_Disconnect_EndsStreamAndUnregisters(t *testing.T) {
	agentID := "44444444-4444-4444-4444-444444444444"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond)

	require.True(t, registry.Disconnect(agentID))

	// The client's Recv must return rather than block forever.
	recvErr := make(chan error, 1)
	go func() { _, e := stream.Recv(); recvErr <- e }()
	select {
	case e := <-recvErr:
		require.Error(t, e)
		assert.Equal(t, codes.PermissionDenied, status.Code(e))
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open 5s after Disconnect — the handler never returned")
	}

	// The handler's deferred Unregister only runs once it returns.
	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		3*time.Second, 20*time.Millisecond,
		"registry entry leaked: the handler goroutine is still alive")
}

// A revoked agent has no reason to keep reading the stream, and once it stops
// the send goroutine parks inside stream.Send — where cancelling the context
// cannot reach it. An earlier version waited for that goroutine before
// returning, which reproduced the very leak IC-BUG-25 is about: the handler
// never returned, the registry entry stayed, IsOnline stayed true.
//
// The agent here deliberately never calls Recv.
func TestServer_Connect_Disconnect_WhileSendBlocked(t *testing.T) {
	agentID := "55555555-5555-5555-5555-555555555555"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		3*time.Second, 10*time.Millisecond)

	// Fill the flow-control window so the send goroutine is parked in Send.
	// SendCh refusing a message is the signal that it has stopped draining.
	big := strings.Repeat("x", 1<<20)
	sent := 0
	for ; sent < 200; sent++ {
		if !registry.Send(agentID, &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_PushRule{
				PushRule: &agentv1.PushRuleCommand{
					Rule: &agentv1.CollectionRule{RuleId: "r", BasePath: big},
				},
			},
		}) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Without this the test silently degrades into a duplicate of the plain
	// disconnect case: if everything fits, the send path never blocked and the
	// scenario under test never happened.
	require.Less(t, sent, 200, "send path never became blocked; nothing was tested")

	require.True(t, registry.Disconnect(agentID))

	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		8*time.Second, 50*time.Millisecond,
		"handler did not return while the send path was blocked — registry and goroutine leaked")
}

// The mirror of the case above: an agent can park the send goroutine and then
// half-close, which drives the receive loop into its error branch. Waiting for
// the send goroutine there pins the handler outside the select, so it stops
// observing ctx.Done entirely and revocation can no longer reach it.
func TestServer_Connect_HalfClose_WhileSendBlocked_StillUnregisters(t *testing.T) {
	agentID := "66666666-6666-6666-6666-666666666666"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		3*time.Second, 10*time.Millisecond)

	big := strings.Repeat("x", 1<<20)
	sent := 0
	for ; sent < 200; sent++ {
		if !registry.Send(agentID, &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_PushRule{
				PushRule: &agentv1.PushRuleCommand{
					Rule: &agentv1.CollectionRule{RuleId: "r", BasePath: big},
				},
			},
		}) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.Less(t, sent, 200, "send path never became blocked; nothing was tested")

	// Half-close: the server's Recv returns EOF and takes the error branch.
	require.NoError(t, stream.CloseSend())

	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		10*time.Second, 50*time.Millisecond,
		"handler pinned in the Recv error branch — the agent made itself unrevokable")
}

// IC-BUG-30 (D-033 hardening): a failed rule sync must END the stream. The
// rules_sync full-set message is the entire delete half of the fix — if it is
// dropped and the agent keeps running, its rule view silently falls back to
// the pre-fix behaviour with no signal. Running with an untrustworthy rule
// view is worse than disconnecting: the agent reconnects into a clean full
// re-sync.
func TestServer_Connect_RuleSyncFailure_EndsStream(t *testing.T) {
	agentID := "77777777-7777-7777-7777-777777777777"
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(agentID, "org-1", "agent", agentID, time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	srv := New(logger)
	srv.WithDeps(registry, newMockCache(), jwtSvc, &mockNATS{}, nil)
	srv.WithExtraDeps(&mockDispatcher{err: assert.AnError}, nil, nil, nil)
	srv.WithStateDB(&mockStateDB{agentStatus: db.AgentStatusApproved})

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	client := agentv1.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))

	// The handler must return after the sync failure, ending the RPC — the
	// client's Recv must come back with an error rather than block forever.
	recvErr := make(chan error, 1)
	go func() { _, e := stream.Recv(); recvErr <- e }()
	select {
	case e := <-recvErr:
		require.Error(t, e, "a failed rule sync must end the stream, not be swallowed")
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open 5s after rule-sync failure — the sync error was swallowed")
	}
	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		3*time.Second, 20*time.Millisecond, "registry entry must be cleaned up")
}

// ── IC-BUG-31 结构半边：≥33 条规则的送达（行为级）─────────────────────────────
//
// ⚠️ 旧形态的实证（决定本测试形状的证据，勿删）：初版实现是「逐条 push 全量 +
// 最后一条 rules_sync + 凭据」共 41 条消息，全部经 32 缓冲的非阻塞 Send 入队。
// 即使发送 goroutine 已提前启动（消费者先于生产者），40 条的 burst 仍然
// 5/5 确定性失败：恰好送达 32 条、其余 8 条与凭据全被 select/default 静默丢弃
// （生产是微秒级紧循环，消费是逐条 stream.Send 含 gRPC 帧封装 + 流控，
// 生产速率恒大于消费速率——这是结构性丢失，不是 flaky）。
// 因此 D-033 改为快照形态：一次同步只发 1 条 RulesSyncCommand，
// 逐条 PushRuleCommand 只留给单条增量下发。本测试由此改证快照路径不丢。
//
// bulkSyncDispatcher reproduces the sync as Connect drives it: one snapshot
// message with every rule, then the credentials push behind it.
type bulkSyncDispatcher struct {
	registry *AgentRegistry
	rules    int
}

func (d *bulkSyncDispatcher) SyncRulesOnConnect(_ context.Context, agentID string) error {
	snapshot := make([]*agentv1.CollectionRule, 0, d.rules)
	for i := 0; i < d.rules; i++ {
		snapshot = append(snapshot, &agentv1.CollectionRule{RuleId: fmt.Sprintf("rule-%02d", i)})
	}
	d.registry.Send(agentID, &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_RulesSync{
			RulesSync: &agentv1.RulesSyncCommand{Rules: snapshot},
		},
	})
	// Mirror pushCredentials: the credentials push follows the rule sync.
	d.registry.Send(agentID, &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_Credentials{
			Credentials: &agentv1.CredentialsPayload{AccessKey: "AKID"},
		},
	})
	return nil
}

// 40 active rules must ALL reach the agent, and the credentials push behind
// them must arrive. 40 > 32 is deliberate — it is exactly the count that
// structurally dropped 8 rules and the credentials under the old per-message
// push form (see the comment above); under the snapshot form the same 40
// arrive as one message with room to spare.
func TestServer_Connect_FortyRules_AllDelivered(t *testing.T) {
	agentID := "88888888-8888-8888-8888-888888888888"
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(agentID, "org-1", "agent", agentID, time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	srv := New(logger)
	srv.WithDeps(registry, newMockCache(), jwtSvc, &mockNATS{}, nil)
	srv.WithExtraDeps(&bulkSyncDispatcher{registry: registry, rules: 40}, nil, nil, nil)
	srv.WithStateDB(&mockStateDB{agentStatus: db.AgentStatusApproved})

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	client := agentv1.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
	require.NoError(t, err)

	rules := make(map[string]bool)
	gotCreds := false
	for {
		msg, rErr := stream.Recv()
		if rErr != nil {
			break
		}
		switch p := msg.GetPayload().(type) {
		case *agentv1.ServerMessage_RulesSync:
			for _, r := range p.RulesSync.GetRules() {
				rules[r.GetRuleId()] = true
			}
		case *agentv1.ServerMessage_Credentials:
			gotCreds = true
		}
		if len(rules) == 40 && gotCreds {
			break
		}
	}
	assert.Len(t, rules, 40, "every pushed rule must reach the agent")
	assert.True(t, gotCreds, "the credentials push behind the rules must arrive")
}

// ── F4（IC-2b review）：钉住「消费者先于生产者启动」的行为级用例 ────────────────
//
// 快照形态下一次同步只有 1 条消息，「消费者后置」变异没有消息丢失的行为差异，
// 40 条快照用例杀不死它。本用例钉住顺序本身的行为签名：同步期间把 SendCh 打满，
// 第 33 条只有在**存在正在排空的消费者**时才可能被接受——
//   消费者先于同步启动（现状）：33 条立即被接受，全部送达（结构性保证，无时序运气）；
//   消费者被移回同步之后（变异）：同步期间永远没有消费者 → 2s 内第 33 条必然
//   不被接受 → SyncRulesOnConnect 返回错误 → Connect 结束流 → 用例红。

// consumerPinningDispatcher 模拟任意在同步期间入队的下发方：打满缓冲后，
// 要求观察到排空才继续，最后再补一条凭据（复刻 sync + pushCredentials 两股生产）。
type consumerPinningDispatcher struct {
	registry *AgentRegistry
	total    int // 要送达的规则消息数，必须 > sendChCapacity
}

func (d *consumerPinningDispatcher) SyncRulesOnConnect(ctx context.Context, agentID string) error {
	pushRule := func(i int) *agentv1.ServerMessage {
		return &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_PushRule{
				PushRule: &agentv1.PushRuleCommand{
					Rule: &agentv1.CollectionRule{RuleId: fmt.Sprintf("rule-%02d", i)},
				},
			},
		}
	}
	for i := 0; i < sendChCapacity; i++ {
		if !d.registry.Send(agentID, pushRule(i)) {
			return fmt.Errorf("buffer rejected message %d with no burst — unexpected", i)
		}
	}
	deadline := time.After(2 * time.Second)
	for i := sendChCapacity; i < d.total; i++ {
		for {
			if d.registry.Send(agentID, pushRule(i)) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline:
				return fmt.Errorf(
					"no consumer drained SendCh during the sync: send goroutine started after the producers (IC-BUG-31)")
			case <-time.After(time.Millisecond):
			}
		}
	}
	d.registry.Send(agentID, &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_Credentials{
			Credentials: &agentv1.CredentialsPayload{AccessKey: "AKID"},
		},
	})
	return nil
}

// The send goroutine must be RUNNING before SyncRulesOnConnect is invoked:
// the dispatcher is only able to deliver past the buffer capacity if a
// consumer is actively draining. This is a structural guarantee, not a timing
// bet — with the consumer started late the sync can never complete.
func TestServer_Connect_SendConsumerRunsBeforeSync(t *testing.T) {
	const total = sendChCapacity + 8
	agentID := "99999999-9999-9999-9999-999999999999"
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(agentID, "org-1", "agent", agentID, time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	srv := New(logger)
	srv.WithDeps(registry, newMockCache(), jwtSvc, &mockNATS{}, nil)
	srv.WithExtraDeps(&consumerPinningDispatcher{registry: registry, total: total}, nil, nil, nil)
	srv.WithStateDB(&mockStateDB{agentStatus: db.AgentStatusApproved})

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	client := agentv1.NewAgentServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
	require.NoError(t, err)

	rules := make(map[string]bool)
	gotCreds := false
	for {
		msg, rErr := stream.Recv()
		if rErr != nil {
			break
		}
		switch p := msg.GetPayload().(type) {
		case *agentv1.ServerMessage_PushRule:
			rules[p.PushRule.GetRule().GetRuleId()] = true
		case *agentv1.ServerMessage_Credentials:
			gotCreds = true
		}
		if len(rules) == total && gotCreds {
			break
		}
	}
	assert.Len(t, rules, total,
		"every message enqueued during the sync must be delivered — which requires the send consumer to run before the sync")
	assert.True(t, gotCreds)
}
