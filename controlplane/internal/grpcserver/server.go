// Package grpcserver provides the gRPC server skeleton for the Control Plane.
// All AgentService methods are wired up here. Phase 1 provides the structural
// foundation; the business logic is filled in during Phase 2.
package grpcserver

import (
	"fmt"
	"net"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// Server holds dependencies shared by all gRPC handlers.
type Server struct {
	// Embed the generated Unimplemented guard so that adding new RPC methods to
	// the proto does not break compilation.
	agentv1.UnimplementedAgentServiceServer

	logger *zap.Logger
	// Additional dependencies (cache, db, etc.) are added in Phase 2.
}

// New creates a new gRPC Server with the provided dependencies.
func New(logger *zap.Logger) *Server {
	return &Server{logger: logger}
}

// Run creates a TCP listener on the given port, registers the AgentService,
// attaches interceptors and starts serving. It blocks until the server is
// stopped or an error occurs.
func (s *Server) Run(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("grpcserver: listen on port %d: %w", port, err)
	}

	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			loggingUnaryInterceptor(s.logger),
			jwtUnaryInterceptor(s.logger),
		),
		grpc.ChainStreamInterceptor(
			loggingStreamInterceptor(s.logger),
			jwtStreamInterceptor(s.logger),
		),
	)

	agentv1.RegisterAgentServiceServer(grpcSrv, s)
	// Enable gRPC server reflection so tools like grpcurl work during development.
	reflection.Register(grpcSrv)

	s.logger.Info("gRPC server listening", zap.Int("port", port))
	return grpcSrv.Serve(lis)
}

// GRPCServer builds and returns a configured *grpc.Server without starting it.
// Useful in tests where the caller controls the listener.
func (s *Server) GRPCServer() *grpc.Server {
	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			loggingUnaryInterceptor(s.logger),
			jwtUnaryInterceptor(s.logger),
		),
		grpc.ChainStreamInterceptor(
			loggingStreamInterceptor(s.logger),
			jwtStreamInterceptor(s.logger),
		),
	)
	agentv1.RegisterAgentServiceServer(grpcSrv, s)
	reflection.Register(grpcSrv)
	return grpcSrv
}
