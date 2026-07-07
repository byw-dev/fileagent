package storage

import (
	"context"
	"fmt"
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

// IssueCredentials obtains temporary STS credentials for an agent.
func (m *STSManager) IssueCredentials(ctx context.Context, agentID string, buckets []BucketAccess) (*agentv1.CredentialsPayload, error) {
	policyJSON, err := BuildSessionPolicy(buckets)
	if err != nil {
		return nil, err
	}

	scheme := "http"
	if m.useSSL {
		scheme = "https"
	}
	stsEndpoint := scheme + "://" + m.endpoint

	li, err := credentials.NewSTSAssumeRole(stsEndpoint, credentials.STSAssumeRoleOptions{
		AccessKey:       m.accessKey,
		SecretKey:       m.secretKey,
		RoleARN:         m.roleARN,
		RoleSessionName: "agent-" + agentID,
		Policy:          policyJSON,
		DurationSeconds: 3600,
	})
	if err != nil {
		return nil, fmt.Errorf("sts: create assume role credential: %w", err)
	}

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
