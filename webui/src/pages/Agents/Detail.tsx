import { Typography } from 'antd'
import { useParams } from 'react-router-dom'

/**
 * Agent detail page (placeholder).
 */
function AgentDetailPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <Typography.Title level={3}>采集器详情</Typography.Title>
      <Typography.Text type="secondary">ID: {id}（页面开发中 Phase 2）</Typography.Text>
    </div>
  )
}

export default AgentDetailPage
