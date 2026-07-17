import { Button, Space, Table, Typography, App } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { useParams, useNavigate } from 'react-router-dom'
import useSWR from 'swr'
import { listAgentUploadLogs } from '../../services/agents'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'

const { Title } = Typography

type UploadLogRow = {
  id: string
  filename: string
  size: number
  status: string
  uploaded_at: string
  error_message?: string
}

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * Agent upload logs standalone page — shows upload history for a specific agent.
 */
function AgentLogsPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { message } = App.useApp()

  const { data, isLoading } = useSWR(
    id ? ['agent-logs', id] : null,
    () => listAgentUploadLogs(id!, { limit: 100 }),
    { onError: () => message.error('获取上传日志失败') },
  )
  const logs = (data?.items ?? []) as UploadLogRow[]
  const loading = isLoading

  const columns: ColumnsType<UploadLogRow> = [
    {
      title: '文件名',
      dataIndex: 'filename',
      key: 'filename',
      ellipsis: true,
    },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 100,
      render: (v: number) => formatBytes(v),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (v: string) => <StatusBadge status={v} domain="upload" />,
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 150,
      render: (v: string) => <TimeText value={v} />,
    },
    {
      title: '错误信息',
      dataIndex: 'error_message',
      key: 'error_message',
      ellipsis: true,
      render: (v: string | undefined) => v ?? '—',
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button onClick={() => navigate(`/agents/${id}`)}>← 返回详情</Button>
        <Title level={3} style={{ margin: 0 }}>上传日志</Title>
        <Typography.Text type="secondary">采集器 {id}</Typography.Text>
      </Space>

      <Table
        dataSource={logs}
        columns={columns}
        rowKey="id"
        loading={loading}
        pagination={{ pageSize: 20 }}
        locale={{ emptyText: <EmptyState description="暂无上传记录" /> }}
      />
    </div>
  )
}

export default AgentLogsPage
