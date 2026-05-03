import { useState } from 'react'
import { Button, Space, Typography, Select, DatePicker, Input, message, Tag } from 'antd'
import { DownloadOutlined, SearchOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useNavigate } from 'react-router-dom'
import { useRef } from 'react'
import { listFiles, getFileDownloadUrl } from '../../services/files'
import type { FileEntry } from '../../services/files'
import BatchDownload from '../../components/BatchDownload'
import type { RangePickerProps } from 'antd/es/date-picker'

const { Title } = Typography
const { RangePicker } = DatePicker

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

const STATUS_OPTIONS = [
  { label: '全部', value: '' },
  { label: '已索引', value: 'INDEXED' },
  { label: '待处理', value: 'PENDING' },
  { label: '错误', value: 'ERROR' },
]

/**
 * File browser page — multi-filter ProTable with single file download and
 * batch download via BatchDownload component.
 */
function FilesPage() {
  const navigate = useNavigate()
  const actionRef = useRef<ActionType | undefined>(undefined)

  const [selectedRowKeys, setSelectedRowKeys] = useState<React.Key[]>([])
  const [filterStatus, setFilterStatus] = useState('')
  const [filterFilename, setFilterFilename] = useState('')
  const [filterRange, setFilterRange] = useState<[string, string] | null>(null)

  const handleDownloadSingle = async (file: FileEntry) => {
    try {
      const { url } = await getFileDownloadUrl(file.id)
      const a = document.createElement('a')
      a.href = url
      a.download = file.filename
      document.body.appendChild(a)
      a.click()
      document.body.removeChild(a)
    } catch {
      message.error('获取下载链接失败')
    }
  }

  const handleRangeChange: RangePickerProps['onChange'] = (_, dateStrings) => {
    if (dateStrings[0] && dateStrings[1]) {
      setFilterRange([dateStrings[0], dateStrings[1]])
    } else {
      setFilterRange(null)
    }
    actionRef.current?.reload()
  }

  const columns: ProColumns<FileEntry>[] = [
    {
      title: '文件名',
      dataIndex: 'filename',
      key: 'filename',
      ellipsis: true,
      render: (_, file) => (
        <a onClick={() => navigate(`/files/${file.id}`)}>{file.filename}</a>
      ),
    },
    {
      title: '原始路径',
      dataIndex: 'original_path',
      key: 'original_path',
      ellipsis: true,
      width: 220,
    },
    {
      title: 'MIME 类型',
      dataIndex: 'mime_type',
      key: 'mime_type',
      width: 140,
    },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 100,
      render: (_, file) => formatBytes(file.size),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 90,
      render: (_, file) => {
        const colorMap: Record<string, string> = {
          INDEXED: 'green',
          PENDING: 'gold',
          ERROR: 'red',
        }
        return <Tag color={colorMap[file.status] ?? 'default'}>{file.status}</Tag>
      },
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 170,
      render: (_, file) => new Date(file.uploaded_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 90,
      render: (_, file) => (
        <Button
          size="small"
          icon={<DownloadOutlined />}
          onClick={() => handleDownloadSingle(file)}
        >
          下载
        </Button>
      ),
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>文件浏览器</Title>

      {/* Filters */}
      <Space wrap style={{ marginBottom: 16 }}>
        <Input
          placeholder="文件名搜索"
          prefix={<SearchOutlined />}
          value={filterFilename}
          onChange={(e) => setFilterFilename(e.target.value)}
          onPressEnter={() => actionRef.current?.reload()}
          style={{ width: 200 }}
          allowClear
        />
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
        <RangePicker
          showTime={false}
          onChange={handleRangeChange}
          placeholder={['开始日期', '结束日期']}
        />
        <BatchDownload fileIds={selectedRowKeys as string[]} />
      </Space>

      <ProTable<FileEntry>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        rowSelection={{
          selectedRowKeys,
          onChange: (keys) => setSelectedRowKeys(keys),
        }}
        request={async () => {
          try {
            const params: Record<string, unknown> = { limit: 100 }
            if (filterStatus) params.status = filterStatus
            if (filterFilename) params.filename = filterFilename
            if (filterRange) {
              params.since = filterRange[0]
              params.until = filterRange[1]
            }
            const data = await listFiles(params as Parameters<typeof listFiles>[0])
            return { data: data.items, success: true, total: data.total }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
        params={{ filterStatus, filterFilename, filterRange }}
      />
    </div>
  )
}

export default FilesPage
