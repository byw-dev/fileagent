import { Empty } from 'antd'
import type { ReactNode } from 'react'

interface EmptyStateProps {
  /** One-line explanation of the empty state (交互定则 6). */
  description: ReactNode
  /** Optional primary action (e.g. a 新建 button). */
  action?: ReactNode
}

/**
 * EmptyState — the site-wide empty state: a one-line description plus an
 * optional primary action (交互定则 6). Replaces AntD's bare English "No data".
 */
function EmptyState({ description, action }: EmptyStateProps) {
  return (
    <Empty
      image={Empty.PRESENTED_IMAGE_SIMPLE}
      description={description}
      style={{ padding: '32px 0' }}
    >
      {action}
    </Empty>
  )
}

export default EmptyState
