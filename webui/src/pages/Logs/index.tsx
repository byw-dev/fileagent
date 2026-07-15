import { useRef, useState } from 'react'
import { Space, Tag, Typography, Descriptions } from 'antd'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { listUploadLogs } from '../../services/upload-logs'
import type { UploadLog } from '../../services/upload-logs'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'

const { Title, Text } = Typography
const { CheckableTag } = Tag

// Upload logs are terminal (completed/failed only) — there is no pending state,
// so the filter offers just those two plus 全部. Values match the API status
// (upper-case); the CP normalizes case server-side.
const STATUS_FILTERS = [
  { label: '全部', value: '' },
  { label: '成功', value: 'COMPLETED' },
  { label: '失败', value: 'FAILED' },
]

/** Format bytes to human-readable size; '—' for missing/invalid input. */
function formatBytes(bytes?: number): string {
  if (bytes == null || !Number.isFinite(bytes)) return '—'
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * Global upload logs page (WR-6). Status filter is a single-line chip row
 * (4e / 交互定则 3); status uses the site-wide badge; failed rows expand to show
 * the error and retry trail (retry count / transferred bytes / timing).
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
      render: (_, log) => (log.agent_id ?? '').slice(0, 8) + '…',
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
      render: (_, log) => <StatusBadge status={log.status} domain="upload" />,
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 150,
      render: (_, log) => <TimeText value={log.uploaded_at} />,
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>上传日志</Title>

      {/* Single-line chip filter (4e / 交互定则 3). */}
      <Space style={{ marginBottom: 16 }} size={4}>
        {STATUS_FILTERS.map((f) => (
          <CheckableTag
            key={f.value}
            checked={filterStatus === f.value}
            onChange={() => {
              setFilterStatus(f.value)
              actionRef.current?.reload()
            }}
          >
            {f.label}
          </CheckableTag>
        ))}
      </Space>

      <ProTable<UploadLog>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20 }}
        expandable={{
          // Failed rows carry an error + retry trail worth expanding.
          rowExpandable: (log) => log.status === 'FAILED',
          expandedRowRender: (log) => (
            <Descriptions size="small" column={1} style={{ margin: 0 }}>
              <Descriptions.Item label="错误信息">
                {log.error_message || <Text type="secondary">—</Text>}
              </Descriptions.Item>
              <Descriptions.Item label="重试次数">
                {log.retry_count ?? <Text type="secondary">—</Text>}
              </Descriptions.Item>
              <Descriptions.Item label="已传输">
                {formatBytes(log.bytes_transferred)} / {formatBytes(log.size)}
              </Descriptions.Item>
              <Descriptions.Item label="开始 / 结束">
                <TimeText value={log.started_at} /> → <TimeText value={log.finished_at} />
              </Descriptions.Item>
            </Descriptions>
          ),
        }}
        request={async () => {
          try {
            const params: Parameters<typeof listUploadLogs>[0] = { limit: 100 }
            if (filterStatus) params.status = filterStatus
            const data = await listUploadLogs(params)
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
