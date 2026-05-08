import { useState } from 'react'
import { Select, Space, Typography, Tag } from 'antd'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useRef } from 'react'
import { listUploadLogs } from '../../services/upload-logs'
import type { UploadLog } from '../../services/upload-logs'

const { Title } = Typography

const STATUS_OPTIONS = [
  { label: '全部', value: '' },
  { label: '成功', value: 'SUCCESS' },
  { label: '失败', value: 'FAILED' },
  { label: '待处理', value: 'PENDING' },
]

const STATUS_COLOR: Record<string, string> = {
  SUCCESS: 'green',
  FAILED: 'red',
  PENDING: 'gold',
}

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * Global upload logs page — shows all upload logs with status filter.
 */
function LogsPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  const [filterStatus, setFilterStatus] = useState('')

  const columns: ProColumns<UploadLog>[] = [
    {
      title: '文件名',
      dataIndex: 'filename',
      key: 'filename',
      ellipsis: true,
    },
    {
      title: '采集器 ID',
      dataIndex: 'agent_id',
      key: 'agent_id',
      width: 140,
      render: (_, log) => log.agent_id.slice(0, 8) + '…',
    },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 100,
      render: (_, log) => formatBytes(log.size),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (_, log) => (
        <Tag color={STATUS_COLOR[log.status] ?? 'default'}>{log.status}</Tag>
      ),
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 170,
      render: (_, log) => new Date(log.uploaded_at).toLocaleString('zh-CN'),
    },
    {
      title: '错误信息',
      dataIndex: 'error_message',
      key: 'error_message',
      ellipsis: true,
      render: (_, log) => log.error_message ?? '—',
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>上传日志</Title>

      <Space style={{ marginBottom: 16 }}>
        <Select
          value={filterStatus}
          onChange={(v) => {
            setFilterStatus(v)
            actionRef.current?.reload()
          }}
          options={STATUS_OPTIONS}
          style={{ width: 120 }}
          placeholder="状态筛选"
        />
      </Space>

      <ProTable<UploadLog>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        request={async () => {
          try {
            const params: Record<string, unknown> = { limit: 100 }
            if (filterStatus) params.status = filterStatus
            const data = await listUploadLogs(params as Parameters<typeof listUploadLogs>[0])
            return { data: data.items, success: true, total: data.total }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
        params={{ filterStatus }}
      />
    </div>
  )
}

export default LogsPage
