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
type STSManager struct {
	endpoint  string
	accessKey string
	secretKey string
	roleARN   string
	useSSL    bool
	logger    *zap.Logger
}

// NewSTSManager creates a new STSManager.
func NewSTSManager(endpoint, accessKey, secretKey, roleARN string, useSSL bool, logger *zap.Logger) *STSManager {
	return &STSManager{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		roleARN:   roleARN,
		useSSL:    useSSL,
		logger:    logger,
	}
}

// IssueCredentials obtains temporary STS credentials for an agent.
func (m *STSManager) IssueCredentials(ctx context.Context, agentID string, buckets []BucketAccess) (*agentv1.CredentialsPayload, error) {
	policyJSON := BuildSessionPolicy(buckets)

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
		Endpoint:     m.endpoint,
		UseSsl:       m.useSSL,
		ExpiresAt:    timestamppb.New(expiresAt),
	}, nil
}
