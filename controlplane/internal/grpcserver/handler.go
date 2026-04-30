package grpcserver

import (
	"context"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
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
		s.publishEvent("events.agent.offline", agentID)
		s.logger.Info("agent disconnected", zap.String("agent_id", agentID))
	}()

	// Mark agent as online in Redis.
	if s.cache != nil {
		if err := s.cache.Set(ctx, cache.AgentOnlineKey(agentID), "1", agentOnlineTTL); err != nil {
			s.logger.Warn("connect: set online key failed", zap.Error(err))
		}
	}
	s.publishEvent("events.agent.online", agentID)
	s.logger.Info("agent connected", zap.String("agent_id", agentID))

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

// RefreshCredentials allows an Agent to request new STS credentials.
func (s *Server) RefreshCredentials(ctx context.Context, req *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	s.logger.Debug("RefreshCredentials called (unimplemented)", zap.String("agent_id", req.GetAgentId()))
	return nil, status.Error(codes.Unimplemented, "RefreshCredentials not yet implemented")
}

// handleAgentMessage processes a single incoming message from an agent.
func (s *Server) handleAgentMessage(ctx context.Context, agentID string, msg *agentv1.AgentMessage) {
	switch p := msg.Payload.(type) {
	case *agentv1.AgentMessage_Heartbeat:
		s.handleHeartbeat(ctx, agentID, p.Heartbeat)
	case *agentv1.AgentMessage_UploadResult:
		s.handleUploadResult(ctx, agentID, p.UploadResult)
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
	s.logger.Debug("heartbeat received",
		zap.String("agent_id", agentID),
		zap.Int64("uptime_seconds", hb.GetUptimeSeconds()),
	)
}

func (s *Server) handleUploadResult(ctx context.Context, agentID string, result *agentv1.UploadResult) {
	s.logger.Info("upload result received",
		zap.String("agent_id", agentID),
		zap.String("storage_path", result.GetStoragePath()),
		zap.Bool("success", result.GetSuccess()),
	)
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
