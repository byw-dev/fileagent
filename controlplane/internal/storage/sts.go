package storage

import (
	"context"
	"fmt"
	"net/http"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// STSManager issues temporary STS credentials for agent uploads.
//
// endpoint is the INTERNAL MinIO endpoint the Control Plane dials to perform the
// STS AssumeRole call. publicEndpoint is the CLIENT-FACING endpoint returned to
// the agent in the CredentialsPayload (the host the agent uploads to / that
// presigned URLs are signed for). They differ when MinIO is reached over an
// in-cluster name internally but a gateway/host address externally; by default
// publicEndpoint mirrors endpoint (see DECISIONS.md D-024).
type STSManager struct {
	endpoint       string
	publicEndpoint string
	accessKey      string
	secretKey      string
	roleARN        string
	useSSL         bool
	publicUseSSL   bool
	logger         *zap.Logger
}

// NewSTSManager creates a new STSManager. The public endpoint defaults to the
// internal endpoint; call WithPublicEndpoint to override it for split
// internal/public deployments.
func NewSTSManager(endpoint, accessKey, secretKey, roleARN string, useSSL bool, logger *zap.Logger) *STSManager {
	return &STSManager{
		endpoint:       endpoint,
		publicEndpoint: endpoint,
		accessKey:      accessKey,
		secretKey:      secretKey,
		roleARN:        roleARN,
		useSSL:         useSSL,
		publicUseSSL:   useSSL,
		logger:         logger,
	}
}

// WithPublicEndpoint overrides the client-facing endpoint (and its TLS flag)
// returned to agents, leaving the internal endpoint used for AssumeRole
// untouched. It returns the receiver for fluent construction.
func (m *STSManager) WithPublicEndpoint(endpoint string, useSSL bool) *STSManager {
	m.publicEndpoint = endpoint
	m.publicUseSSL = useSSL
	return m
}

// stsRequestTimeout bounds a single AssumeRole call.
//
// The call is made on the Connect path, so an unreachable MinIO that blackholes
// packets (rather than refusing them) would otherwise block stream setup
// indefinitely and starve the agent's heartbeat until Redis marked it offline.
// minio-go's AssumeRole flow takes no context, so the bound has to come from the
// HTTP client.
const stsRequestTimeout = 10 * time.Second

// IssueCredentials obtains temporary STS credentials for an agent.
//
// ctx is honoured for cancellation before the call; the AssumeRole request
// itself is bounded by stsRequestTimeout because the upstream library offers no
// context-aware entry point.
func (m *STSManager) IssueCredentials(ctx context.Context, agentID string, buckets []BucketAccess) (*agentv1.CredentialsPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sts: %w", err)
	}

	policyJSON, err := BuildSessionPolicy(buckets)
	if err != nil {
		return nil, err
	}

	scheme := "http"
	if m.useSSL {
		scheme = "https"
	}
	stsEndpoint := scheme + "://" + m.endpoint

	if m.accessKey == "" || m.secretKey == "" {
		return nil, fmt.Errorf("sts: access key and secret key are required")
	}
	li := credentials.New(&credentials.STSAssumeRole{
		STSEndpoint: stsEndpoint,
		Options: credentials.STSAssumeRoleOptions{
			AccessKey:       m.accessKey,
			SecretKey:       m.secretKey,
			RoleARN:         m.roleARN,
			RoleSessionName: "agent-" + agentID,
			Policy:          policyJSON,
			DurationSeconds: 3600,
		},
		Client: &http.Client{Timeout: stsRequestTimeout},
	})

	val, err := li.Get()
	if err != nil {
		return nil, fmt.Errorf("sts: get credentials: %w", err)
	}

	m.logger.Info("STS credentials issued",
		zap.String("agent_id", agentID),
		zap.String("access_key_id", val.AccessKeyID),
	)

	expiresAt := time.Now().Add(time.Hour)
	return &agentv1.CredentialsPayload{
		AccessKey:    val.AccessKeyID,
		SecretKey:    val.SecretAccessKey,
		SessionToken: val.SessionToken,
		Endpoint:     m.publicEndpoint,
		UseSsl:       m.publicUseSSL,
		ExpiresAt:    timestamppb.New(expiresAt),
	}, nil
}
