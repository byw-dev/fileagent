import { Typography } from 'antd'
import { useParams } from 'react-router-dom'

/**
 * Event deliveries history page (placeholder).
 */
function EventDeliveriesPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <Typography.Title level={3}>事件投递历史</Typography.Title>
      <Typography.Text type="secondary">规则 {id}（页面开发中 Phase 2）</Typography.Text>
    </div>
  )
}

export default EventDeliveriesPage
