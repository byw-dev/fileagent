// Package worker holds Control Plane background reconciliation loops.
//
// OfflineSweeper is the TTL-driven offline fallback for agent presence. The
// gRPC handler marks an agent offline in its stream-disconnect defer, but that
// defer never runs when the Control Plane crashes/restarts or the TCP connection
// half-opens. In those cases the Redis presence key (AgentOnlineKey, 90s TTL
// refreshed by heartbeats) still expires, but the persistent agents.status stays
// "online" forever and the events.agent.offline event is never published. The
// sweeper reconciles that: it periodically marks agents whose presence key has
// expired as offline and republishes the offline event (system-design §5.2).
package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// defaultSweepInterval is how often OfflineSweeper.Run reconciles when no
// interval is supplied. Presence keys carry a 90s TTL, so a 30s sweep bounds the
// stale-online window to roughly one TTL plus one interval.
const defaultSweepInterval = 30 * time.Second

// agentOfflineSubject and the offlineEvent payload mirror the gRPC disconnect
// path (grpcserver.publishEvent) so downstream event rules observe identical
// offline events regardless of which path detected the disconnect.
const agentOfflineSubject = "events.agent.offline"

type offlineEvent struct {
	AgentID string `json:"agent_id"`
}

// StatusDB is the subset of DB queries the sweeper needs.
//
// MarkAgentOfflineIfOnline transitions an agent to offline only when it is still
// online and returns the number of rows affected, so the sweeper publishes the
// offline event exactly once even if the gRPC disconnect path marked the agent
// offline first.
type StatusDB interface {
	ListAgentsByStatus(ctx context.Context, orgID uuid.UUID, status db.AgentStatus) ([]*db.Agent, error)
	MarkAgentOfflineIfOnline(ctx context.Context, id uuid.UUID) (int64, error)
}

// PresenceCache exposes the Redis existence check for presence keys.
type PresenceCache interface {
	Exists(ctx context.Context, keys ...string) (int64, error)
}

// EventPublisher publishes NATS events.
type EventPublisher interface {
	Publish(subject string, data []byte) error
}

// OfflineSweeper reconciles persistent agent status against Redis presence TTL.
//
// It assumes a single Control Plane instance (the v1 deployment model), so no
// distributed lock is used. Enforcement is best-effort: an agent that reconnects
// in the small window between the presence check and the conditional status
// update may be transiently marked offline. That window is microseconds and the
// conditional update (MarkAgentOfflineIfOnline) already prevents duplicating the
// disconnect path's event; a reconnected agent's presence key is present, so it
// is skipped on the next sweep regardless.
type OfflineSweeper struct {
	db        StatusDB
	cache     PresenceCache
	publisher EventPublisher
	orgID     uuid.UUID
	logger    *zap.Logger
}

// NewOfflineSweeper constructs an OfflineSweeper scoped to a single organisation.
func NewOfflineSweeper(sdb StatusDB, c PresenceCache, p EventPublisher, orgID uuid.UUID, logger *zap.Logger) *OfflineSweeper {
	return &OfflineSweeper{db: sdb, cache: c, publisher: p, orgID: orgID, logger: logger}
}

// Sweep marks every online agent whose Redis presence key has expired as offline
// and publishes events.agent.offline for each. It returns the number of agents
// transitioned. Per-agent errors are logged and skipped so one failure does not
// abort the whole pass.
func (s *OfflineSweeper) Sweep(ctx context.Context) int {
	agents, err := s.db.ListAgentsByStatus(ctx, s.orgID, db.AgentStatusOnline)
	if err != nil {
		if ctx.Err() != nil {
			return 0 // shutdown in progress: a cancelled context is not an error
		}
		s.logger.Warn("offline sweep: list online agents failed", zap.Error(err))
		return 0
	}

	var swept int
	for _, a := range agents {
		if ctx.Err() != nil {
			return swept // stop promptly on shutdown instead of failing every call
		}
		if !s.presenceExpired(ctx, a.ID) {
			continue
		}
		// Conditional transition: only mark offline if still online. If the gRPC
		// disconnect path already flipped it (and published), rows == 0 and we
		// stay quiet — no duplicate events.agent.offline.
		rows, err := s.db.MarkAgentOfflineIfOnline(ctx, a.ID)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("offline sweep: mark offline failed",
					zap.String("agent_id", a.ID.String()), zap.Error(err))
			}
			continue
		}
		if rows == 0 {
			continue // already offline elsewhere; do not re-publish
		}
		s.publishOffline(a.ID.String())
		swept++
		s.logger.Info("offline sweep: agent marked offline (presence expired)",
			zap.String("agent_id", a.ID.String()))
	}
	return swept
}

// presenceExpired reports whether the agent's Redis presence key is gone with a
// single EXISTS. On a cache error it returns false (fail-safe: never mark an
// agent offline when presence is unknown), and it stays quiet when the error is
// just a cancelled context during shutdown.
func (s *OfflineSweeper) presenceExpired(ctx context.Context, id uuid.UUID) bool {
	n, err := s.cache.Exists(ctx, cache.AgentOnlineKey(id.String()))
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("offline sweep: presence check failed",
				zap.String("agent_id", id.String()), zap.Error(err))
		}
		return false
	}
	return n == 0
}

func (s *OfflineSweeper) publishOffline(agentID string) {
	payload, err := json.Marshal(offlineEvent{AgentID: agentID})
	if err != nil { // unreachable for a fixed struct, but stay explicit
		s.logger.Error("offline sweep: marshal offline event failed",
			zap.String("agent_id", agentID), zap.Error(err))
		return
	}
	if err := s.publisher.Publish(agentOfflineSubject, payload); err != nil {
		s.logger.Error("offline sweep: publish agent.offline failed",
			zap.String("agent_id", agentID), zap.Error(err))
	}
}

// Run sweeps on a ticker until ctx is cancelled. A non-positive interval falls
// back to defaultSweepInterval.
func (s *OfflineSweeper) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.logger.Info("offline sweeper started", zap.Duration("interval", interval))
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("offline sweeper stopped")
			return
		case <-ticker.C:
			// ctx.Done() and ticker.C can be ready simultaneously; skip the tick
			// if shutdown has started rather than sweeping with a cancelled ctx.
			if ctx.Err() != nil {
				s.logger.Info("offline sweeper stopped")
				return
			}
			s.Sweep(ctx)
		}
	}
}
