package grpcserver

import (
	"context"
	"encoding/json"
	"fmt"
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
			if _, dbErr := s.stateDB.UpdateAgentStatus(ctx, id, db.AgentStatusOnline); dbErr != nil {
				s.logger.Warn("connect: update status to online failed", zap.Error(dbErr))
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
	for {
		msg, err := stream.Recv()
		if err != nil {
			cancel()
			<-sendErr
			return err
		}
		s.handleAgentMessage(ctx, agentID, msg)
	}
}

// RefreshCredentials allows an Agent to request new STS credentials for a
// specific collection rule.
func (s *Server) RefreshCredentials(ctx context.Context, req *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	if s.stsMgr == nil || s.credDB == nil {
		s.logger.Debug("RefreshCredentials called (stsMgr/credDB not wired)",
			zap.String("agent_id", req.GetAgentId()))
		return nil, status.Error(codes.Unimplemented, "RefreshCredentials not yet implemented")
	}

	agentID := req.GetAgentId()
	ruleID := req.GetRuleId()

	// Look up collection rule to find the target bucket.
	parsedRuleID, err := uuid.Parse(ruleID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid rule_id: %v", err)
	}

	rule, err := s.credDB.GetCollectionRuleByID(ctx, parsedRuleID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "collection rule not found: %v", err)
	}

	bucket, err := s.credDB.GetBucketByID(ctx, rule.BucketID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "bucket not found: %v", err)
	}

	creds, err := s.stsMgr.IssueCredentials(ctx, agentID, []storage.BucketAccess{
		{BucketName: bucket.Name, PathPrefix: fmt.Sprintf("agents/%s/", agentID)},
	})
	if err != nil {
		s.logger.Error("refresh_credentials: issue STS failed",
			zap.String("agent_id", agentID),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to issue credentials: %v", err)
	}

	s.logger.Info("credentials refreshed",
		zap.String("agent_id", agentID),
		zap.String("bucket", bucket.Name),
	)
	return &agentv1.RefreshCredentialsResponse{Credentials: creds}, nil
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
		s.handleDryRunResult(p.DryRunResult)
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
			// is not online (e.g. the offline sweeper's reconnect-race false
			// positive), restore it and publish a corrective online event. The
			// conditional update is a no-op (0 rows) in steady state, so this does
			// not spam events on every heartbeat.
			if rows, dbErr := s.stateDB.MarkAgentOnlineIfNotOnline(ctx, id); dbErr != nil {
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
		s.logger.Warn("disconnect: update status to offline failed", zap.Error(dbErr))
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
func (s *Server) handleDryRunResult(result *agentv1.DryRunResult) {
	if s.dryRunStore == nil {
		s.logger.Debug("dry_run result received but no dryRunStore wired",
			zap.String("rule_id", result.GetRuleId()))
		return
	}
	s.dryRunStore.Deliver(result.GetRuleId(), result)
	s.logger.Debug("dry_run result delivered",
		zap.String("rule_id", result.GetRuleId()),
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
