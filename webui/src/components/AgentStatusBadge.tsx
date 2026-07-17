import StatusBadge from './StatusBadge'

/** Supported agent status values (frontend upper-case; see contracts.md V-1). */
export type AgentStatus = 'PENDING' | 'APPROVED' | 'RUNNING' | 'OFFLINE' | 'REVOKED'

interface AgentStatusBadgeProps {
  /** Agent lifecycle status string. */
  status: AgentStatus | string
  /**
   * Real-time online flag (Redis heartbeat TTL). When provided, it is the source
   * of truth for the online/offline distinction: a RUNNING/OFFLINE agent renders
   * 在线/离线 by `isOnline`, not by the possibly-stale DB status. This keeps the
   * list and detail views showing one consistent status (fixes the 口径不一致 bug).
   */
  isOnline?: boolean
}

/**
 * Effective agent status: for the connected lifecycle phase (RUNNING/OFFLINE),
 * the real-time `isOnline` decides 在线 vs 离线; PENDING/APPROVED/REVOKED are
 * shown as-is.
 */
function effectiveAgentStatus(
  status: AgentStatus | string,
  isOnline?: boolean,
): AgentStatus | string {
  if (isOnline !== undefined && (status === 'RUNNING' || status === 'OFFLINE')) {
    return isOnline ? 'RUNNING' : 'OFFLINE'
  }
  return status
}

/**
 * AgentStatusBadge — thin wrapper over the site-wide {@link StatusBadge} that
 * reconciles the DB lifecycle status with the real-time online flag so every
 * view shows the same caliber. New code should pass `isOnline` when available.
 */
function AgentStatusBadge({ status, isOnline }: AgentStatusBadgeProps) {
  return <StatusBadge status={effectiveAgentStatus(status, isOnline)} domain="agent" />
}

export default AgentStatusBadge
