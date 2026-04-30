package grpcserver

import (
	"context"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Register handles the initial Agent registration request.
// Phase 1: returns Unimplemented — business logic added in Phase 2 (T2-A2).
func (s *Server) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	s.logger.Debug("Register called (unimplemented)", zap.String("fingerprint", req.GetFingerprint()))
	return nil, status.Error(codes.Unimplemented, "Register not yet implemented")
}

// PollApproval lets an Agent poll for its approval status.
// Phase 1: returns Unimplemented — business logic added in Phase 2 (T2-A2).
func (s *Server) PollApproval(ctx context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	s.logger.Debug("PollApproval called (unimplemented)", zap.String("agent_id", req.GetAgentId()))
	return nil, status.Error(codes.Unimplemented, "PollApproval not yet implemented")
}

// Connect establishes the bidirectional streaming connection used by an
// approved Agent to receive commands and send events.
// Phase 1: returns Unimplemented — business logic added in Phase 2 (T2-A3).
func (s *Server) Connect(stream grpc.BidiStreamingServer[agentv1.AgentMessage, agentv1.ServerMessage]) error {
	s.logger.Debug("Connect called (unimplemented)")
	return status.Error(codes.Unimplemented, "Connect not yet implemented")
}

// RefreshCredentials allows an Agent to request new STS credentials before
// the current ones expire.
// Phase 1: returns Unimplemented — business logic added in Phase 2 (T2-A5).
func (s *Server) RefreshCredentials(ctx context.Context, req *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	s.logger.Debug("RefreshCredentials called (unimplemented)", zap.String("agent_id", req.GetAgentId()))
	return nil, status.Error(codes.Unimplemented, "RefreshCredentials not yet implemented")
}
