// Package agent provides the agent lifecycle manager for the Control Plane.
// It handles registration, approval, revocation, and token management.
package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"go.uber.org/zap"
)

// defaultOrgID is the single-org UUID used in Phase 2 (single-tenant mode).
var defaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// AgentDB is the database interface required by Manager.
type AgentDB interface {
	CreateAgent(ctx context.Context, arg db.CreateAgentParams) (*db.Agent, error)
	GetAgentByFingerprint(ctx context.Context, fingerprint string) (*db.Agent, error)
	GetAgentByID(ctx context.Context, id uuid.UUID) (*db.Agent, error)
	UpdateAgentStatus(ctx context.Context, iD uuid.UUID, status db.AgentStatus) (*db.Agent, error)
	UpdateAgentAuthToken(ctx context.Context, iD uuid.UUID, authTokenHash sql.NullString, tokenExpiresAt sql.NullTime) (*db.Agent, error)
}

// CacheClient is the cache interface required by Manager.
type CacheClient interface {
	Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// NATSPublisher is the messaging interface required by Manager.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// Manager orchestrates agent registration, approval and revocation.
type Manager struct {
	db        AgentDB
	cache     CacheClient
	jwtSvc    auth.Service
	nats      NATSPublisher
	logger    *zap.Logger
	accessTTL time.Duration
}

// NewManager creates a new agent Manager.
func NewManager(
	agentDB AgentDB,
	cacheClient CacheClient,
	jwtSvc auth.Service,
	nats NATSPublisher,
	logger *zap.Logger,
	accessTTL time.Duration,
) *Manager {
	return &Manager{
		db:        agentDB,
		cache:     cacheClient,
		jwtSvc:    jwtSvc,
		nats:      nats,
		logger:    logger,
		accessTTL: accessTTL,
	}
}

// Register handles agent self-registration.
func (m *Manager) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	existing, err := m.db.GetAgentByFingerprint(ctx, req.GetFingerprint())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("register: lookup fingerprint: %w", err)
	}
	if err == nil {
		resp := &agentv1.RegisterResponse{
			AgentId:   existing.ID.String(),
			Status:    string(existing.Status),
			Message:   "Agent already registered",
			AgentName: existing.Name,
		}
		if existing.Status == db.AgentStatusApproved && existing.AuthTokenHash.Valid {
			// Token was already issued at approval time; return status only.
			resp.Message = "Agent already approved"
		}
		return resp, nil
	}

	// Parse IP address, falling back to 127.0.0.1 on parse errors.
	ipStr := req.GetIpAddress()
	if ipStr == "" {
		ipStr = "127.0.0.1"
	}
	parsedIP := net.ParseIP(ipStr)
	if parsedIP == nil {
		m.logger.Warn("invalid IP address from agent, using fallback",
			zap.String("ip", ipStr))
		parsedIP = net.ParseIP("127.0.0.1")
	}

	osInfo, _ := json.Marshal(map[string]string{
		"os_type":       req.GetOsType(),
		"os_version":    req.GetOsVersion(),
		"arch":          req.GetArch(),
		"agent_version": req.GetAgentVersion(),
		"hostname":      req.GetHostname(),
	})

	inet := pqtype.Inet{}
	if err := inet.Scan(parsedIP.String()); err != nil {
		m.logger.Warn("failed to scan IP into inet", zap.Error(err))
		_ = inet.Scan("127.0.0.1")
	}

	agent, err := m.db.CreateAgent(ctx, db.CreateAgentParams{
		OrgID:       defaultOrgID,
		Name:        req.GetHostname(),
		Fingerprint: req.GetFingerprint(),
		OsInfo:      json.RawMessage(osInfo),
		IpAddress:   inet,
	})
	if err != nil {
		return nil, fmt.Errorf("register: create agent: %w", err)
	}

	m.logger.Info("agent registered", zap.String("agent_id", agent.ID.String()))
	return &agentv1.RegisterResponse{
		AgentId:   agent.ID.String(),
		Status:    string(db.AgentStatusPending),
		Message:   "Agent registered. Awaiting approval.",
		AgentName: agent.Name,
	}, nil
}

// PollApproval returns the current approval status for an agent.
func (m *Manager) PollApproval(ctx context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	agentID, err := uuid.Parse(req.GetAgentId())
	if err != nil {
		return nil, fmt.Errorf("poll_approval: invalid agent_id: %w", err)
	}
	agent, err := m.db.GetAgentByID(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("poll_approval: get agent: %w", err)
	}
	resp := &agentv1.PollApprovalResponse{
		Status:    string(agent.Status),
		Message:   statusMessage(agent.Status),
		AgentName: agent.Name,
	}
	if agent.Status != db.AgentStatusApproved {
		return resp, nil
	}
	if m.jwtSvc == nil {
		return nil, fmt.Errorf("poll_approval: jwt service not configured")
	}

	rawToken, err := m.jwtSvc.GenerateAccessToken(
		agent.ID.String(),
		agent.OrgID.String(),
		"agent",
		agent.Name,
		m.accessTTL,
	)
	if err != nil {
		return nil, fmt.Errorf("poll_approval: generate token: %w", err)
	}

	sum := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(sum[:])
	expiresAt := time.Now().Add(m.accessTTL)
	if _, err = m.db.UpdateAgentAuthToken(
		ctx,
		agent.ID,
		sql.NullString{String: hash, Valid: true},
		sql.NullTime{Time: expiresAt, Valid: true},
	); err != nil {
		return nil, fmt.Errorf("poll_approval: update auth token: %w", err)
	}

	resp.AuthToken = rawToken
	return resp, nil
}

// ApproveAgent approves an agent, generates its auth token, and returns the
// raw JWT string.
func (m *Manager) ApproveAgent(ctx context.Context, agentID uuid.UUID, approvedByUserID uuid.UUID) (string, error) {
	agent, err := m.db.GetAgentByID(ctx, agentID)
	if err != nil {
		return "", fmt.Errorf("approve_agent: get agent: %w", err)
	}

	// Generate a long-lived access token for the agent.
	rawToken, err := m.jwtSvc.GenerateAccessToken(
		agentID.String(),
		agent.OrgID.String(),
		"agent",
		agent.Name,
		m.accessTTL,
	)
	if err != nil {
		return "", fmt.Errorf("approve_agent: generate token: %w", err)
	}

	// Store SHA-256 hash of the raw token (never store the raw token).
	sum := sha256.Sum256([]byte(rawToken))
	hash := hex.EncodeToString(sum[:])

	expiresAt := time.Now().Add(m.accessTTL)
	_, err = m.db.UpdateAgentAuthToken(ctx, agentID,
		sql.NullString{String: hash, Valid: true},
		sql.NullTime{Time: expiresAt, Valid: true},
	)
	if err != nil {
		return "", fmt.Errorf("approve_agent: update auth token: %w", err)
	}

	_, err = m.db.UpdateAgentStatus(ctx, agentID, db.AgentStatusApproved)
	if err != nil {
		return "", fmt.Errorf("approve_agent: update status: %w", err)
	}

	m.publishEvent("events.agent.approved", map[string]string{
		"agent_id":    agentID.String(),
		"approved_by": approvedByUserID.String(),
		"agent_name":  agent.Name,
	})

	m.logger.Info("agent approved",
		zap.String("agent_id", agentID.String()),
		zap.String("approved_by", approvedByUserID.String()),
	)
	return rawToken, nil
}

// RevokeAgent revokes an agent, preventing further connections.
func (m *Manager) RevokeAgent(ctx context.Context, agentID uuid.UUID, revokedByUserID uuid.UUID) error {
	agent, err := m.db.GetAgentByID(ctx, agentID)
	if err != nil {
		return fmt.Errorf("revoke_agent: get agent: %w", err)
	}

	_, err = m.db.UpdateAgentStatus(ctx, agentID, db.AgentStatusRevoked)
	if err != nil {
		return fmt.Errorf("revoke_agent: update status: %w", err)
	}

	// Remove online key from cache if present.
	if m.cache != nil {
		_ = m.cache.Del(ctx, cache.AgentOnlineKey(agentID.String()))
	}

	m.publishEvent("events.agent.revoked", map[string]string{
		"agent_id":   agentID.String(),
		"revoked_by": revokedByUserID.String(),
		"agent_name": agent.Name,
	})

	m.logger.Info("agent revoked",
		zap.String("agent_id", agentID.String()),
		zap.String("revoked_by", revokedByUserID.String()),
	)
	return nil
}

func (m *Manager) publishEvent(subject string, payload interface{}) {
	if m.nats == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		m.logger.Error("publish event: marshal payload", zap.Error(err))
		return
	}
	if err := m.nats.Publish(subject, data); err != nil {
		m.logger.Error("publish event: nats publish", zap.String("subject", subject), zap.Error(err))
	}
}

func statusMessage(s db.AgentStatus) string {
	switch s {
	case db.AgentStatusPending:
		return "Awaiting approval"
	case db.AgentStatusApproved:
		return "Approved"
	case db.AgentStatusOnline:
		return "Online"
	case db.AgentStatusOffline:
		return "Offline"
	case db.AgentStatusRevoked:
		return "Revoked"
	default:
		return string(s)
	}
}
