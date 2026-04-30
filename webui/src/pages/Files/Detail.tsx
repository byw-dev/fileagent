import { Typography } from 'antd'
import { useParams } from 'react-router-dom'

/**
 * File detail page (placeholder).
 */
function FileDetailPage() {
  const { id } = useParams<{ id: string }>()
  return (
    <div>
      <Typography.Title level={3}>文件详情</Typography.Title>
      <Typography.Text type="secondary">ID: {id}（页面开发中 Phase 2）</Typography.Text>
    </div>
  )
}

export default FileDetailPage
