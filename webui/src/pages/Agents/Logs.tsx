import { Typography } from 'antd'
import { useParams } from 'react-router-dom'

/**
 * Agent logs page (placeholder).
 */
function AgentLogsPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <Typography.Title level={3}>采集器日志</Typography.Title>
      <Typography.Text type="secondary">采集器 {id}（页面开发中 Phase 2）</Typography.Text>
    </div>
  )
}

export default AgentLogsPage
