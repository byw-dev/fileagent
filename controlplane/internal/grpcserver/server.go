// Package grpcserver provides the gRPC server skeleton for the Control Plane.
// All AgentService methods are wired up here. Phase 1 provides the structural
// foundation; the business logic is filled in during Phase 2.
package grpcserver

import (
	"context"
	"fmt"
	"net"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// CacheClient is the cache interface required by the gRPC server.
type CacheClient interface {
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// NATSPublisher is the messaging interface required by the gRPC server.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// AgentManager is the interface used by gRPC handlers to delegate agent
// lifecycle operations.
type AgentManager interface {
	Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error)
	PollApproval(ctx context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error)
}

// DispatcherClient is the interface used by Connect to sync rules on reconnect.
type DispatcherClient interface {
	SyncRulesOnConnect(ctx context.Context, agentID string) error
}

// IndexerClient is the interface used by handleUploadResult.
type IndexerClient interface {
	HandleUploadResult(ctx context.Context, agentID uuid.UUID, orgID uuid.UUID, result *agentv1.UploadResult) error
}

// STSManagerClient is the interface used by RefreshCredentials.
type STSManagerClient interface {
	IssueCredentials(ctx context.Context, agentID string, buckets []storage.BucketAccess) (*agentv1.CredentialsPayload, error)
}

// CredentialDB is the minimal DB interface used by RefreshCredentials to look
// up rule and bucket details.
type CredentialDB interface {
	GetCollectionRuleByID(ctx context.Context, id uuid.UUID) (*db.CollectionRule, error)
	GetBucketByID(ctx context.Context, id uuid.UUID) (*db.Bucket, error)
}

// AgentStateDB is the minimal DB interface used by Connect/Disconnect and
// handleHeartbeat to persist agent lifecycle state.
type AgentStateDB interface {
	UpdateAgentLastSeen(ctx context.Context, id uuid.UUID) error
	UpdateAgentStatus(ctx context.Context, id uuid.UUID, status db.AgentStatus) (*db.Agent, error)
}

// DirResultDeliverer receives directory-listing results from the agent gRPC
// stream and delivers them to the waiting REST handler.
type DirResultDeliverer interface {
	Deliver(requestID string, result dirstore.Result)
}

// DryRunResultDeliverer receives dry-run results from the agent gRPC stream
// and delivers them to the waiting REST handler.
type DryRunResultDeliverer interface {
	Deliver(reqID string, result *agentv1.DryRunResult)
}

// Server holds dependencies shared by all gRPC handlers.
type Server struct {
	// Embed the generated Unimplemented guard so that adding new RPC methods to
	// the proto does not break compilation.
	agentv1.UnimplementedAgentServiceServer

	logger        *zap.Logger
	registry      *AgentRegistry
	cache         CacheClient
	jwtSvc        auth.Service
	nats          NATSPublisher
	agentMgr      AgentManager
	dispatcher    DispatcherClient
	indexer       IndexerClient
	stsMgr        STSManagerClient
	credDB        CredentialDB
	stateDB       AgentStateDB
	dirResultStore  DirResultDeliverer
	dryRunStore     DryRunResultDeliverer
}

// New creates a new gRPC Server with the provided logger. Additional
// dependencies can be injected via WithDeps / WithExtraDeps.
func New(logger *zap.Logger) *Server {
	return &Server{logger: logger}
}

// WithDeps injects the core dependencies into the server.
func (s *Server) WithDeps(
	registry *AgentRegistry,
	cache CacheClient,
	jwtSvc auth.Service,
	nats NATSPublisher,
	agentMgr AgentManager,
) *Server {
	s.registry = registry
	s.cache = cache
	s.jwtSvc = jwtSvc
	s.nats = nats
	s.agentMgr = agentMgr
	return s
}

// WithExtraDeps injects the Phase-2 business-logic dependencies.
func (s *Server) WithExtraDeps(
	dispatcher DispatcherClient,
	ix IndexerClient,
	stsMgr STSManagerClient,
	credDB CredentialDB,
) *Server {
	s.dispatcher = dispatcher
	s.indexer = ix
	s.stsMgr = stsMgr
	s.credDB = credDB
	return s
}

// WithStateDB injects the AgentStateDB used to persist agent lifecycle state.
func (s *Server) WithStateDB(stateDB AgentStateDB) *Server {
	s.stateDB = stateDB
	return s
}

// WithDirResultStore injects the store used to deliver directory listing
// results from the gRPC receive loop to the waiting REST handler.
func (s *Server) WithDirResultStore(store DirResultDeliverer) *Server {
	s.dirResultStore = store
	return s
}

// WithDryRunStore injects the store used to deliver dry-run results from the
// gRPC receive loop to the waiting REST handler.
func (s *Server) WithDryRunStore(store DryRunResultDeliverer) *Server {
	s.dryRunStore = store
	return s
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
			jwtUnaryInterceptor(s.logger, s.jwtSvc),
		),
		grpc.ChainStreamInterceptor(
			loggingStreamInterceptor(s.logger),
			jwtStreamInterceptor(s.logger, s.jwtSvc),
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
			jwtUnaryInterceptor(s.logger, s.jwtSvc),
		),
		grpc.ChainStreamInterceptor(
			loggingStreamInterceptor(s.logger),
			jwtStreamInterceptor(s.logger, s.jwtSvc),
		),
	)
	agentv1.RegisterAgentServiceServer(grpcSrv, s)
	reflection.Register(grpcSrv)
	return grpcSrv
}
