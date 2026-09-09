package grpcserver

import (
	"context"
	"encoding/json"
	"path"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const agentOnlineTTL = 90 * time.Second

// Register handles the initial Agent registration request.
func (s *Server) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	if s.agentMgr == nil {
		s.logger.Debug("Register called (unimplemented)", zap.String("fingerprint", req.GetFingerprint()))
		return nil, status.Error(codes.Unimplemented, "Register not yet implemented")
	}
	return s.agentMgr.Register(ctx, req)
}

// PollApproval lets an Agent poll for its approval status.
func (s *Server) PollApproval(ctx context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	if s.agentMgr == nil {
		s.logger.Debug("PollApproval called (unimplemented)", zap.String("agent_id", req.GetAgentId()))
		return nil, status.Error(codes.Unimplemented, "PollApproval not yet implemented")
	}
	return s.agentMgr.PollApproval(ctx, req)
}

// Connect establishes the bidirectional streaming connection used by an
// approved Agent to receive commands and send events.
func (s *Server) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.ServerMessage]) error {
	if s.registry == nil {
		s.logger.Debug("Connect called (unimplemented)")
		return status.Error(codes.Unimplemented, "Connect not yet implemented")
	}

	// Extract the agent ID from JWT claims stored in context.
	agentID := extractAgentID(stream.Context())
	if agentID == "" {
		return status.Error(codes.Unauthenticated, "missing agent identity in token")
	}

	// A revoked agent keeps a syntactically valid JWT until it expires (default
	// 30 days), and revocation never invalidated it. Checking the persisted
	// status here is what actually makes RevokeAgent take effect.
	if err := s.assertAgentUsable(stream.Context(), agentID); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(stream.Context())
	conn := s.registry.Register(agentID, stream, cancel)
	defer func() {
		s.registry.Unregister(agentID)
		if s.cache != nil {
			_ = s.cache.Del(context.Background(), cache.AgentOnlineKey(agentID))
		}
		s.markOfflineOnDisconnect(agentID)
		s.logger.Info("agent disconnected", zap.String("agent_id", agentID))
	}()

	// Mark agent as online in Redis.
	if s.cache != nil {
		if err := s.cache.Set(ctx, cache.AgentOnlineKey(agentID), "1", agentOnlineTTL); err != nil {
			s.logger.Warn("connect: set online key failed", zap.Error(err))
		}
	}
	if s.stateDB != nil {
		if id, err := uuid.Parse(agentID); err == nil {
			// Constrained in SQL rather than relying on the liveness check above:
			// a revocation landing between the two would otherwise be undone by
			// an unconditional write, and the agent would then pass the gate on
			// every subsequent call. Terminal states must never be revived.
			rows, dbErr := s.stateDB.MarkAgentOnlineIfUsable(ctx, id)
			switch {
			case dbErr != nil:
				s.logger.Warn("connect: update status to online failed", zap.Error(dbErr))
			case rows == 0:
				// The status left the usable set between the liveness check and
				// this write — i.e. the agent was revoked mid-connect. The
				// constraint did its job; log it, because this is the only place
				// that race is ever visible.
				s.logger.Warn("connect: agent left the usable state during connect setup",
					zap.String("agent_id", agentID))
			}
		}
	}
	s.publishEvent("events.agent.online", agentID)
	s.logger.Info("agent connected", zap.String("agent_id", agentID))

	// Sync all active rules to the freshly connected agent (CP-W3 / CP-W4).
	if s.dispatcher != nil {
		if err := s.dispatcher.SyncRulesOnConnect(ctx, agentID); err != nil {
			s.logger.Warn("connect: sync rules failed", zap.String("agent_id", agentID), zap.Error(err))
		}
	}

	// Push an initial STS session so the agent can upload immediately (IC-BUG-1).
	s.pushCredentials(ctx, agentID)

	// Start send goroutine.
	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				sendErr <- nil
				return
			case msg, ok := <-conn.SendCh:
				if !ok {
					sendErr <- nil
					return
				}
				if err := stream.Send(msg); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()

	// Receive loop.
	//
	// Recv runs in its own goroutine so the loop can also wake on ctx.Done().
	// Reading Recv directly would block until the agent sends something or the
	// transport breaks, which means cancelling ctx — the only thing Disconnect
	// can do — would not end this handler: it would keep its registry entry and
	// its goroutines forever while an uncooperative agent held the stream open.
	// Only returning from this function actually terminates the RPC; the pending
	// Recv then fails and its goroutine exits.
	type recvResult struct {
		msg *agentv1.AgentMessage
		err error
	}
	recvCh := make(chan recvResult)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case recvCh <- recvResult{msg: msg, err: err}:
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Cancelled from outside — the agent was revoked (IC-BUG-25).
			//
			// Deliberately does not wait for the send goroutine. That goroutine
			// only checks ctx while idle; if it is parked inside stream.Send it
			// stays there until the client reads, which a revoked agent has no
			// reason to do. Waiting here would hang this handler exactly as the
			// blocking Recv used to, leaking the goroutine and the registry
			// entry and leaving IsOnline true forever.
			//
			// Returning is what actually ends the RPC: the parked Send then
			// fails, and the goroutine's write to the buffered sendErr channel
			// completes without a reader.
			s.logger.Info("connect: stream terminated by control plane",
				zap.String("agent_id", agentID))
			return status.Error(codes.PermissionDenied, "agent connection terminated")
		case r := <-recvCh:
			if r.err != nil {
				cancel()
				<-sendErr
				return r.err
			}
			s.handleAgentMessage(ctx, agentID, r.msg)
		}
	}
}

// RefreshCredentials allows an Agent to request new STS credentials.
//
// rule_id is optional. When omitted the session covers every bucket targeted by
// the agent's active collection rules, which is what the agent actually needs:
// it uploads for all of its rules from a single credential and has no reason to
// track which rule a queued task came from. Requiring rule_id was IC-BUG-1 —
// the agent never sent one, so the parse failed and no credential was ever
// issued.
func (s *Server) RefreshCredentials(ctx context.Context, req *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	if s.stsMgr == nil || s.credDB == nil {
		s.logger.Debug("RefreshCredentials called (stsMgr/credDB not wired)",
			zap.String("agent_id", req.GetAgentId()))
		return nil, status.Error(codes.Unimplemented, "RefreshCredentials not yet implemented")
	}

	// The identity always comes from the verified JWT claims, never from the
	// request body. request.agent_id is a caller-supplied string; honouring it
	// would let any approved agent mint credentials for another agent's buckets,
	// which under the bucket-wide policy of D-030 §8 means full write access to
	// someone else's data.
	agentID := extractAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing agent identity in token")
	}
	if claimed := req.GetAgentId(); claimed != "" && claimed != agentID {
		s.logger.Warn("refresh_credentials: agent_id does not match token subject",
			zap.String("token_agent_id", agentID),
			zap.String("claimed_agent_id", claimed),
		)
		return nil, status.Error(codes.PermissionDenied, "agent_id does not match authenticated identity")
	}

	if err := s.assertAgentUsable(ctx, agentID); err != nil {
		return nil, err
	}

	buckets, err := s.bucketsForAgent(ctx, agentID, req.GetRuleId())
	if err != nil {
		return nil, err
	}

	creds, err := s.stsMgr.IssueCredentials(ctx, agentID, buckets)
	if err != nil {
		s.logger.Error("refresh_credentials: issue STS failed",
			zap.String("agent_id", agentID),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to issue credentials: %v", err)
	}

	s.logger.Info("credentials refreshed",
		zap.String("agent_id", agentID),
		zap.Int("buckets", len(buckets)),
	)
	return &agentv1.RefreshCredentialsResponse{Credentials: creds}, nil
}

// assertAgentUsable rejects agents that must no longer act, consulting the
// persisted status rather than the token.
//
// Agent JWTs are long-lived (AGENT_TOKEN_TTL defaults to 30 days) and
// RevokeAgent never invalidated them, so without this gate revoking a
// compromised agent depended on that agent voluntarily deleting its own token —
// which is precisely what a compromised agent will not do. Since D-030 §8 an
// agent token buys bucket-wide write access, so revocation has to actually bite.
//
// It fails open when no state DB is wired (unit-test servers), and treats a
// lookup failure as fatal: an agent row that cannot be read must not be trusted.
func (s *Server) assertAgentUsable(ctx context.Context, agentID string) error {
	if s.stateDB == nil {
		return nil
	}
	id, err := uuid.Parse(agentID)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
	}
	agent, err := s.stateDB.GetAgentByID(ctx, id)
	if err != nil {
		s.logger.Warn("agent liveness check failed", zap.String("agent_id", agentID), zap.Error(err))
		return status.Error(codes.PermissionDenied, "agent is not in a usable state")
	}
	switch agent.Status {
	case db.AgentStatusApproved, db.AgentStatusOnline, db.AgentStatusOffline:
		return nil
	default:
		s.logger.Warn("rejected agent in non-usable state",
			zap.String("agent_id", agentID),
			zap.String("status", string(agent.Status)),
		)
		return status.Errorf(codes.PermissionDenied, "agent status is %s", agent.Status)
	}
}

// bucketsForAgent resolves the bucket set an STS session should cover.
//
// agentID must already be the authenticated identity, never a value taken from
// the request body. A non-empty ruleID narrows the session to that rule's
// bucket, but only if the rule belongs to this agent; otherwise every active
// rule of the agent contributes its bucket. The result is deduplicated by
// BuildSessionPolicy.
func (s *Server) bucketsForAgent(ctx context.Context, agentID, ruleID string) ([]storage.BucketAccess, error) {
	parsedAgentID, err := uuid.Parse(agentID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
	}

	if ruleID != "" {
		parsedRuleID, pErr := uuid.Parse(ruleID)
		if pErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid rule_id: %v", pErr)
		}
		rule, rErr := s.credDB.GetCollectionRuleByID(ctx, parsedRuleID)
		if rErr != nil {
			return nil, status.Errorf(codes.NotFound, "collection rule not found: %v", rErr)
		}
		// Ownership check: a rule id is caller-supplied, so without this an
		// agent could name any other agent's rule and receive credentials for
		// that rule's bucket.
		if rule.AgentID != parsedAgentID {
			s.logger.Warn("credentials: rule does not belong to the requesting agent",
				zap.String("agent_id", agentID),
				zap.String("rule_id", ruleID),
			)
			return nil, status.Error(codes.PermissionDenied, "collection rule does not belong to this agent")
		}
		bucket, bErr := s.credDB.GetBucketByID(ctx, rule.BucketID)
		if bErr != nil {
			return nil, status.Errorf(codes.NotFound, "bucket not found: %v", bErr)
		}
		return []storage.BucketAccess{{BucketName: bucket.Name}}, nil
	}
	rules, err := s.credDB.ListCollectionRulesByAgent(ctx, parsedAgentID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list collection rules: %v", err)
	}

	seen := make(map[uuid.UUID]struct{}, len(rules))
	var buckets []storage.BucketAccess
	for _, rule := range rules {
		if rule.Status != db.RuleStatusActive {
			continue
		}
		if _, dup := seen[rule.BucketID]; dup {
			continue
		}
		seen[rule.BucketID] = struct{}{}
		bucket, bErr := s.credDB.GetBucketByID(ctx, rule.BucketID)
		if bErr != nil {
			s.logger.Warn("credentials: lookup bucket failed, skipping",
				zap.String("rule_id", rule.ID.String()),
				zap.Error(bErr))
			continue
		}
		buckets = append(buckets, storage.BucketAccess{BucketName: bucket.Name})
	}
	if len(buckets) == 0 {
		return nil, status.Error(codes.FailedPrecondition,
			"agent has no active collection rule with a resolvable bucket")
	}
	return buckets, nil
}

// pushCredentials issues an STS session for the agent and delivers it over the
// open stream.
//
// The Control Plane never used to send ServerMessage_Credentials at all, so an
// agent that had just connected sat there with no credentials and failed every
// upload (IC-BUG-1). Pushing once at stream setup — alongside the rule sync —
// means the agent is ready to upload as soon as it has rules to act on.
func (s *Server) pushCredentials(ctx context.Context, agentID string) {
	if s.stsMgr == nil || s.credDB == nil {
		return
	}
	buckets, err := s.bucketsForAgent(ctx, agentID, "")
	if err != nil {
		// No active rule yet is the normal state for a freshly approved agent;
		// it will get credentials from the refresh RPC once rules arrive.
		s.logger.Info("connect: no credentials pushed",
			zap.String("agent_id", agentID),
			zap.Error(err))
		return
	}
	creds, err := s.stsMgr.IssueCredentials(ctx, agentID, buckets)
	if err != nil {
		s.logger.Error("connect: issue STS failed",
			zap.String("agent_id", agentID), zap.Error(err))
		return
	}
	if !s.registry.Send(agentID, &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_Credentials{Credentials: creds},
	}) {
		s.logger.Warn("connect: deliver credentials failed",
			zap.String("agent_id", agentID))
		return
	}
	s.logger.Info("connect: credentials pushed",
		zap.String("agent_id", agentID),
		zap.Int("buckets", len(buckets)))
}

// handleAgentMessage processes a single incoming message from an agent.
func (s *Server) handleAgentMessage(ctx context.Context, agentID string, msg *agentv1.AgentMessage) {
	switch p := msg.Payload.(type) {
	case *agentv1.AgentMessage_Heartbeat:
		s.handleHeartbeat(ctx, agentID, p.Heartbeat)
	case *agentv1.AgentMessage_UploadResult:
		s.handleUploadResult(ctx, agentID, p.UploadResult)
	case *agentv1.AgentMessage_DirectoryListing:
		s.handleDirectoryListing(agentID, p.DirectoryListing)
	case *agentv1.AgentMessage_DryRunResult:
		s.handleDryRunResult(ctx, agentID, p.DryRunResult)
	default:
		s.logger.Debug("agent message received",
			zap.String("agent_id", agentID),
			zap.String("message_id", msg.MessageId),
		)
	}
}

func (s *Server) handleHeartbeat(ctx context.Context, agentID string, hb *agentv1.Heartbeat) {
	if s.cache != nil {
		if err := s.cache.Set(ctx, cache.AgentOnlineKey(agentID), "1", agentOnlineTTL); err != nil {
			s.logger.Warn("heartbeat: refresh online TTL failed", zap.Error(err))
		}
	}
	if s.stateDB != nil {
		if id, err := uuid.Parse(agentID); err == nil {
			if dbErr := s.stateDB.UpdateAgentLastSeen(ctx, id); dbErr != nil {
				s.logger.Warn("heartbeat: update last_seen_at failed", zap.Error(dbErr))
			}
			// Self-heal: a heartbeat proves the agent is alive, so if the DB status
			// is offline (e.g. the offline sweeper's reconnect-race false positive),
			// restore it to online and publish a corrective online event. Only
			// offline→online transitions — terminal states like 'revoked'/'pending'
			// are intentionally left untouched. The conditional update is a no-op
			// (0 rows) in steady state, so this does not spam events per heartbeat.
			if rows, dbErr := s.stateDB.MarkAgentOnlineIfOffline(ctx, id); dbErr != nil {
				s.logger.Warn("heartbeat: restore online status failed", zap.Error(dbErr))
			} else if rows > 0 {
				s.logger.Info("heartbeat: restored agent to online", zap.String("agent_id", agentID))
				s.publishEvent("events.agent.online", agentID)
			}
		}
	}
	// Persist the live telemetry snapshot (G-4) so the agents API can surface
	// queue depth / uptime / version. Shares the online TTL, so it disappears
	// when the agent goes offline.
	if s.cache != nil {
		snapshot, err := json.Marshal(agentStats{
			QueueDepth:    hb.GetQueueDepth(),
			UptimeSeconds: hb.GetUptimeSeconds(),
			UploadBps:     hb.GetUploadBps(),
			Version:       hb.GetVersion(),
		})
		if err == nil {
			if setErr := s.cache.Set(ctx, cache.AgentStatsKey(agentID), string(snapshot), agentOnlineTTL); setErr != nil {
				s.logger.Warn("heartbeat: cache stats failed", zap.Error(setErr))
			}
		}
	}
	s.logger.Debug("heartbeat received",
		zap.String("agent_id", agentID),
		zap.Int64("uptime_seconds", hb.GetUptimeSeconds()),
		zap.Int32("queue_depth", hb.GetQueueDepth()),
	)
}

// agentStats is the JSON telemetry snapshot cached per agent from its heartbeat.
type agentStats struct {
	QueueDepth    int32   `json:"queue_depth"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	UploadBps     float32 `json:"upload_bps"`
	Version       string  `json:"version"`
}

// handleUploadResult forwards the upload result to the Indexer for file entry
// creation, upload log recording, and NATS event publishing (CP-W2).
func (s *Server) handleUploadResult(ctx context.Context, agentID string, result *agentv1.UploadResult) {
	s.logger.Info("upload result received",
		zap.String("agent_id", agentID),
		zap.String("storage_path", result.GetStoragePath()),
		zap.Bool("success", result.GetSuccess()),
	)

	if s.indexer == nil {
		return
	}

	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		s.logger.Error("handleUploadResult: invalid agent_id", zap.String("agent_id", agentID), zap.Error(err))
		return
	}

	// Extract orgID from JWT claims in context; fall back to default org for
	// single-org deployments.
	orgID := extractOrgID(ctx)

	if err := s.indexer.HandleUploadResult(ctx, agentUUID, orgID, result); err != nil {
		s.logger.Error("handleUploadResult: indexer failed",
			zap.String("agent_id", agentID),
			zap.Error(err),
		)
	}
}

// markOfflineOnDisconnect transitions the agent to offline on stream disconnect
// and publishes events.agent.offline, but only when it actually transitions from
// online. This mirrors the offline sweeper's conditional update so a disconnect
// does not double-fire the offline event if the sweeper already marked the agent
// offline. When no state DB is wired (e.g. unit tests), it preserves the prior
// always-publish behaviour.
func (s *Server) markOfflineOnDisconnect(agentID string) {
	id, err := uuid.Parse(agentID)
	if s.stateDB == nil || err != nil {
		// No state DB wired, or the agent id is not a UUID (tests / mis-issued
		// token): we cannot do a conditional transition, so preserve the prior
		// always-publish behaviour rather than silently swallowing the disconnect.
		if err != nil {
			s.logger.Warn("disconnect: agent id is not a UUID; publishing offline unconditionally",
				zap.String("agent_id", agentID))
		}
		s.publishEvent("events.agent.offline", agentID)
		return
	}
	rows, dbErr := s.stateDB.MarkAgentOfflineIfOnline(context.Background(), id)
	if dbErr != nil {
		// The transition is unknown, so fall back to the prior always-publish
		// behaviour rather than dropping the offline event on a transient DB error.
		s.logger.Warn("disconnect: update status to offline failed; publishing offline anyway",
			zap.Error(dbErr))
		s.publishEvent("events.agent.offline", agentID)
		return
	}
	if rows > 0 {
		s.publishEvent("events.agent.offline", agentID)
	}
}

func (s *Server) publishEvent(subject, agentID string) {
	if s.nats == nil {
		return
	}
	payload := []byte(`{"agent_id":"` + agentID + `"}`)
	if err := s.nats.Publish(subject, payload); err != nil {
		s.logger.Error("publish event failed",
			zap.String("subject", subject),
			zap.Error(err),
		)
	}
}

// handleDirectoryListing converts a DirectoryListing proto message from the
// agent into a dirstore.Result and delivers it to the waiting REST handler.
func (s *Server) handleDirectoryListing(agentID string, listing *agentv1.DirectoryListing) {
	if s.dirResultStore == nil {
		s.logger.Debug("dir listing received but no dirResultStore wired",
			zap.String("agent_id", agentID),
			zap.String("request_id", listing.GetRequestId()),
		)
		return
	}

	if listing.GetError() != "" {
		s.dirResultStore.Deliver(listing.GetRequestId(), dirstore.Result{
			Error: listing.GetError(),
		})
		return
	}

	basePath := listing.GetPath()
	entries := make([]dirstore.DirEntry, 0, len(listing.GetEntries()))
	for _, e := range listing.GetEntries() {
		entry := dirstore.DirEntry{
			Name:  e.GetName(),
			Path:  path.Join(basePath, e.GetName()),
			IsDir: e.GetIsDir(),
		}
		if !e.GetIsDir() {
			sz := e.GetSizeBytes()
			entry.Size = &sz
		}
		if ts := e.GetModifiedAt(); ts != nil && ts.IsValid() {
			t := ts.AsTime().UTC().Format(time.RFC3339)
			entry.ModifiedAt = &t
		}
		entries = append(entries, entry)
	}

	s.dirResultStore.Deliver(listing.GetRequestId(), dirstore.Result{Entries: entries})
	s.logger.Debug("dir listing delivered",
		zap.String("agent_id", agentID),
		zap.String("request_id", listing.GetRequestId()),
		zap.Int("entries", len(entries)),
	)
}

// handleDryRunResult delivers a dry-run result from the agent to the waiting
// REST handler via the dryRunStore.
func (s *Server) handleDryRunResult(ctx context.Context, agentID string, result *agentv1.DryRunResult) {
	if s.dryRunStore == nil {
		s.logger.Debug("dry_run result received but no dryRunStore wired",
			zap.String("rule_id", result.GetRuleId()))
		return
	}
	// The id here is a correlation id the Control Plane minted for one specific
	// agent, not a collection rule — so the question is "was this request
	// addressed to you", which only the store can answer. Looking the id up as a
	// rule would reject every legitimate result, since it is never persisted.
	if !s.dryRunStore.Deliver(result.GetRuleId(), agentID, result) {
		s.logger.Warn("dry_run result discarded: not addressed to this agent, or already timed out",
			zap.String("agent_id", agentID),
			zap.String("request_id", result.GetRuleId()),
		)
		return
	}
	s.logger.Debug("dry_run result delivered",
		zap.String("agent_id", agentID),
		zap.String("request_id", result.GetRuleId()),
		zap.Int("files", len(result.GetFiles())),
	)
}

// extractAgentID retrieves the agent's subject from JWT claims stored in ctx
// by the gRPC JWT interceptor.
func extractAgentID(ctx context.Context) string {
	v := ctx.Value(claimsContextKey)
	if v == nil {
		return ""
	}
	claims, ok := v.(*auth.Claims)
	if !ok || claims == nil {
		return ""
	}
	return claims.Subject
}

// defaultOrgID is the single-org UUID used in Phase 1 deployments.
var defaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// extractOrgID reads the org_id from JWT claims in context, falling back to
// the default org for single-org deployments.
func extractOrgID(ctx context.Context) uuid.UUID {
	v := ctx.Value(claimsContextKey)
	if v == nil {
		return defaultOrgID
	}
	claims, ok := v.(*auth.Claims)
	if !ok || claims == nil || claims.OrgID == "" {
		return defaultOrgID
	}
	id, err := uuid.Parse(claims.OrgID)
	if err != nil {
		return defaultOrgID
	}
	return id
}
