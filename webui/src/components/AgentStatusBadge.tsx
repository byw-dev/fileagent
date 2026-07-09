import StatusBadge from './StatusBadge'

/** Supported agent status values (frontend upper-case; see contracts.md V-1). */
export type AgentStatus = 'PENDING' | 'APPROVED' | 'RUNNING' | 'OFFLINE' | 'REVOKED'

interface AgentStatusBadgeProps {
  /** Agent status string. */
  status: AgentStatus | string
}

/**
 * AgentStatusBadge — thin wrapper over the site-wide {@link StatusBadge}, kept
 * for backward compatibility with existing agent call sites. New code should
 * use `<StatusBadge>` directly.
 */
function AgentStatusBadge({ status }: AgentStatusBadgeProps) {
  return <StatusBadge status={status} domain="agent" />
}

export default AgentStatusBadge
