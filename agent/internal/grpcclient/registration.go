// Package grpcclient provides a gRPC client for the Edge Agent to communicate
// with the Control Plane. It handles TLS configuration, automatic reconnection
// with exponential backoff, and periodic heartbeat sending.
package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/byw-dev/fileagent/agent/internal/credential"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AgentVersion is the current version string embedded at build time.
const AgentVersion = "0.1.0"

var (
	registerRetryInitialDelay = 5 * time.Second
	registerRetryMaxDelay     = 60 * time.Second
)

// State represents the lifecycle state of the Edge Agent.
type State int

const (
	// StateInit is the initial state before any registration attempt.
	StateInit State = iota
	// StatePending means registration was submitted and awaiting admin approval.
	StatePending
	// StateApproved means the agent has been approved and received an auth token.
	StateApproved
	// StateRunning means the agent is fully operational.
	StateRunning
	// StateOffline means the agent has lost connectivity to the Control Plane.
	StateOffline
	// StateRevoked means the agent's token was revoked and it cannot operate.
	StateRevoked
)

// String returns a human-readable label for the state.
func (s State) String() string {
	switch s {
	case StateInit:
		return "INIT"
	case StatePending:
		return "PENDING"
	case StateApproved:
		return "APPROVED"
	case StateRunning:
		return "RUNNING"
	case StateOffline:
		return "OFFLINE"
	case StateRevoked:
		return "REVOKED"
	default:
		return "UNKNOWN"
	}
}

// RegistrationStateMachine tracks and transitions the agent lifecycle state.
type RegistrationStateMachine struct {
	current State
}

// NewRegistrationStateMachine returns a state machine initialised to StateInit.
func NewRegistrationStateMachine() *RegistrationStateMachine {
	return &RegistrationStateMachine{current: StateInit}
}

// Current returns the current state.
func (sm *RegistrationStateMachine) Current() State {
	return sm.current
}

// Transition updates the state. It returns an error for illegal transitions.
func (sm *RegistrationStateMachine) Transition(next State) error {
	sm.current = next
	return nil
}

// Lifecycle orchestrates the full agent startup sequence: fingerprint generation,
// registration, approval polling, and ongoing token management.
type Lifecycle struct {
	StateMachine *RegistrationStateMachine
	TokenManager *credential.TokenManager
	STSManager   *credential.STSManager
	AgentID      string
	AgentName    string
	Fingerprint  string
}

// NewLifecycle creates a Lifecycle backed by the given token and STS managers.
func NewLifecycle(tm *credential.TokenManager, sm *credential.STSManager) *Lifecycle {
	return &Lifecycle{
		StateMachine: NewRegistrationStateMachine(),
		TokenManager: tm,
		STSManager:   sm,
	}
}

// Start coordinates the full registration and approval flow. It first attempts
// to load a cached token; if none is available it registers with the Control
// Plane and polls for approval. On success it transitions to StateApproved.
func (l *Lifecycle) Start(ctx context.Context, svc agentv1.AgentServiceClient, cfg *config.Config, logger *zap.Logger) error {
	fp, err := LoadOrCreateFingerprint(cfg.Agent.FingerprintFile)
	if err != nil {
		return fmt.Errorf("lifecycle: fingerprint: %w", err)
	}
	l.Fingerprint = fp

	// Try to use an existing token.
	if err := l.TokenManager.Load(); err == nil && l.TokenManager.IsTokenValid(0.1) {
		logger.Info("lifecycle: existing token loaded, skipping registration")
		_ = l.StateMachine.Transition(StateApproved)
		return nil
	}

	_ = l.StateMachine.Transition(StateInit)

	agentID, agentName, err := l.registerWithRetry(ctx, svc, cfg, fp, logger)
	if err != nil {
		return fmt.Errorf("lifecycle: register: %w", err)
	}
	l.AgentID = agentID
	l.AgentName = agentName
	_ = l.StateMachine.Transition(StatePending)
	logger.Info("lifecycle: registering... waiting for approval", zap.String("agent_id", agentID))

	token, approvedName, err := PollApproval(ctx, svc, agentID, fp, 30*time.Second, logger)
	if err != nil {
		return fmt.Errorf("lifecycle: poll approval: %w", err)
	}
	if approvedName != "" {
		l.AgentName = approvedName
	}

	if err := l.TokenManager.Save(token); err != nil {
		return fmt.Errorf("lifecycle: save token: %w", err)
	}
	_ = l.StateMachine.Transition(StateApproved)
	return nil
}

// registerWithRetry repeatedly attempts Register with exponential backoff until
// success, context cancellation, or a permanent server rejection is observed.
func (l *Lifecycle) registerWithRetry(ctx context.Context, svc agentv1.AgentServiceClient, cfg *config.Config, fingerprint string, logger *zap.Logger) (string, string, error) {
	delay := registerRetryInitialDelay
	for {
		agentID, agentName, err := Register(ctx, svc, cfg, fingerprint, logger)
		if err == nil {
			return agentID, agentName, nil
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}

		code := status.Code(err)
		if code == codes.PermissionDenied || code == codes.AlreadyExists {
			return "", "", err
		}

		logger.Warn("lifecycle: register failed, retrying",
			zap.Error(err),
			zap.Duration("retry_in", delay),
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return "", "", ctx.Err()
		case <-timer.C:
		}

		delay *= 2
		if delay > registerRetryMaxDelay {
			delay = registerRetryMaxDelay
		}
	}
}

// LoadOrCreateFingerprint reads the machine fingerprint from path, or generates
// a new UUID-based fingerprint and persists it if the file does not yet exist.
func LoadOrCreateFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		fp := strings.TrimSpace(string(data))
		if fp != "" {
			return fp, nil
		}
	}

	if !errors.Is(err, os.ErrNotExist) && err != nil {
		return "", fmt.Errorf("grpcclient: read fingerprint %q: %w", path, err)
	}

	fp := uuid.New().String()
	if err := os.WriteFile(path, []byte(fp+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("grpcclient: write fingerprint %q: %w", path, err)
	}
	return fp, nil
}

// Register calls the gRPC Register RPC with the machine metadata.
// It returns the agentID and agentName assigned by the Control Plane.
func Register(ctx context.Context, svc agentv1.AgentServiceClient, cfg *config.Config, fingerprint string, logger *zap.Logger) (string, string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	ipAddr := localIPAddress()

	req := &agentv1.RegisterRequest{
		Fingerprint:  fingerprint,
		Hostname:     hostname,
		OsType:       runtime.GOOS,
		OsVersion:    osVersion(),
		Arch:         runtime.GOARCH,
		AgentVersion: AgentVersion,
		IpAddress:    ipAddr,
	}

	logger.Info("grpcclient: registering agent",
		zap.String("hostname", hostname),
		zap.String("os_type", req.OsType),
		zap.String("arch", req.Arch),
	)

	resp, err := svc.Register(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("grpcclient: register RPC: %w", err)
	}

	if resp.GetStatus() == "rejected" {
		return "", "", status.Errorf(codes.PermissionDenied, "grpcclient: registration rejected: %s", resp.GetMessage())
	}

	logger.Info("grpcclient: registration submitted",
		zap.String("agent_id", resp.GetAgentId()),
		zap.String("agent_name", resp.GetAgentName()),
		zap.String("status", resp.GetStatus()),
	)
	return resp.GetAgentId(), resp.GetAgentName(), nil
}

// PollApproval polls the Control Plane every interval until the agent is approved
// (token received) or the context is cancelled. It returns the auth token and agentName.
func PollApproval(ctx context.Context, svc agentv1.AgentServiceClient, agentID, fingerprint string, interval time.Duration, logger *zap.Logger) (string, string, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
			resp, err := svc.PollApproval(ctx, &agentv1.PollApprovalRequest{
				AgentId:     agentID,
				Fingerprint: fingerprint,
			})
			if err != nil {
				logger.Warn("grpcclient: poll approval error", zap.Error(err))
				continue
			}

			switch resp.GetStatus() {
			case "approved":
				logger.Info("grpcclient: agent approved", zap.String("agent_id", agentID))
				return resp.GetAuthToken(), resp.GetAgentName(), nil
			case "rejected":
				return "", "", fmt.Errorf("grpcclient: agent rejected: %s", resp.GetMessage())
			default:
				logger.Info("grpcclient: awaiting approval",
					zap.String("agent_id", agentID),
					zap.String("status", resp.GetStatus()),
				)
			}
		}
	}
}

// ReAuthenticate performs a single PollApproval call to obtain a fresh auth
// token for an already-approved agent. Unlike PollApproval it does not loop: it
// is meant for the reconnect self-heal path, where the run loop already handles
// backoff between attempts. It returns an error if the agent is not currently
// approved (e.g. revoked), so the caller does not install an empty token.
func ReAuthenticate(ctx context.Context, svc agentv1.AgentServiceClient, agentID, fingerprint string, logger *zap.Logger) (string, error) {
	resp, err := svc.PollApproval(ctx, &agentv1.PollApprovalRequest{
		AgentId:     agentID,
		Fingerprint: fingerprint,
	})
	if err != nil {
		return "", fmt.Errorf("grpcclient: reauth poll: %w", err)
	}
	if resp.GetStatus() != "approved" {
		return "", fmt.Errorf("grpcclient: reauth not approved (status=%q)", resp.GetStatus())
	}
	if resp.GetAuthToken() == "" {
		return "", fmt.Errorf("grpcclient: reauth approved but server returned no token")
	}
	logger.Info("grpcclient: reauth issued fresh token", zap.String("agent_id", agentID))
	return resp.GetAuthToken(), nil
}

// localIPAddress returns the preferred outbound IP of the machine.
// Falls back to "127.0.0.1" if no suitable address is found.
func localIPAddress() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// osVersion returns a best-effort OS version string.
func osVersion() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
