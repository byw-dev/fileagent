import { Typography } from 'antd'
import { useParams } from 'react-router-dom'

/**
 * Agent rules list page (placeholder).
 */
function AgentRulesPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <Typography.Title level={3}>采集规则</Typography.Title>
      <Typography.Text type="secondary">采集器 {id}（页面开发中 Phase 2）</Typography.Text>
    </div>
  )
}

export default AgentRulesPage
