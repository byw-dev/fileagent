import { useState, useEffect } from 'react'
import { Button, Space, Table, Tag, Typography, message, Spin } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { useParams, useNavigate } from 'react-router-dom'
import { listRuleDeliveries } from '../../services/events'
import type { EventDelivery } from '../../services/events'

const { Title } = Typography

const STATUS_COLOR: Record<string, string> = {
  SUCCESS: 'green',
  FAILED: 'red',
  PENDING: 'gold',
}

/**
 * Event deliveries page — shows delivery records for a specific event rule.
 */
function EventDeliveriesPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()

  const [deliveries, setDeliveries] = useState<EventDelivery[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    if (!id) return
    setLoading(true)
    listRuleDeliveries(id, { limit: 50 })
      .then((res) => setDeliveries(res.data))
      .catch(() => message.error('获取投递记录失败'))
      .finally(() => setLoading(false))
  }, [id])

  const columns: ColumnsType<EventDelivery> = [
    {
      title: '投递 ID',
      dataIndex: 'id',
      key: 'id',
      ellipsis: true,
      width: 280,
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (status: string) => (
        <Tag color={STATUS_COLOR[status] ?? 'default'}>{status}</Tag>
      ),
    },
    {
      title: '尝试次数',
      dataIndex: 'attempt_count',
      key: 'attempt_count',
      width: 100,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (v: string) => new Date(v).toLocaleString('zh-CN'),
    },
  ]

  if (loading) {
    return (
      <Spin size="large" style={{ display: 'block', textAlign: 'center', marginTop: 80 }} />
    )
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button onClick={() => navigate('/events')}>← 返回规则列表</Button>
        <Title level={3} style={{ margin: 0 }}>
          投递记录
        </Title>
        <Typography.Text type="secondary">规则 ID：{id}</Typography.Text>
      </Space>

      <Table
        dataSource={deliveries}
        columns={columns}
        rowKey="id"
        pagination={{ pageSize: 20 }}
        locale={{ emptyText: '暂无投递记录' }}
      />
    </div>
  )
}

export default EventDeliveriesPage
