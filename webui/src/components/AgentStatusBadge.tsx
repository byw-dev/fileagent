import { Tag } from 'antd'
import type { TagProps } from 'antd'

/** Supported agent status values. */
export type AgentStatus =
  | 'PENDING'
  | 'APPROVED'
  | 'RUNNING'
  | 'OFFLINE'
  | 'REVOKED'

const STATUS_CONFIG: Record<AgentStatus, { color: TagProps['color']; label: string }> = {
  PENDING: { color: 'gold', label: '待审批' },
  APPROVED: { color: 'blue', label: '已审批' },
  RUNNING: { color: 'green', label: '运行中' },
  OFFLINE: { color: 'default', label: '离线' },
  REVOKED: { color: 'red', label: '已吊销' },
}

interface AgentStatusBadgeProps {
  /** Agent status string. */
  status: AgentStatus
}

/**
 * AgentStatusBadge — renders a colored Ant Design Tag for an agent status.
 */
function AgentStatusBadge({ status }: AgentStatusBadgeProps) {
  const config = STATUS_CONFIG[status] ?? { color: 'default', label: status }
  return <Tag color={config.color}>{config.label}</Tag>
}

export default AgentStatusBadge
