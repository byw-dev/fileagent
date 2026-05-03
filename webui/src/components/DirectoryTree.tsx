import { Table, Button, Space, Typography } from 'antd'
import { FolderOutlined, FileOutlined, ArrowLeftOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'

/** Remote directory entry returned by the list-dir API. */
export interface DirEntry {
  name: string
  path: string
  is_dir: boolean
  size: number | null
  modified_at: string | null
}

interface DirectoryTreeProps {
  /** Flat list of directory entries for the current path. */
  entries: DirEntry[]
  /** Current browsing path. */
  currentPath: string
  /** Callback when the user navigates into a sub-directory or parent. */
  onNavigate: (path: string) => void
}

/** Format bytes to human-readable size */
function formatBytes(bytes: number | null): string {
  if (bytes === null) return '-'
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * DirectoryTree — flat table view of a remote directory listing.
 * Supports navigating into sub-directories and back to the parent.
 */
function DirectoryTree({ entries, currentPath, onNavigate }: DirectoryTreeProps) {
  const columns: ColumnsType<DirEntry> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (name: string, entry) => (
        <Space>
          {entry.is_dir ? (
            <FolderOutlined style={{ color: '#faad14' }} />
          ) : (
            <FileOutlined style={{ color: '#8c8c8c' }} />
          )}
          {entry.is_dir ? (
            <a onClick={() => onNavigate(entry.path)}>{name}</a>
          ) : (
            <span>{name}</span>
          )}
        </Space>
      ),
    },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 100,
      render: (size: number | null, entry) =>
        entry.is_dir ? '-' : formatBytes(size),
    },
    {
      title: '修改时间',
      dataIndex: 'modified_at',
      key: 'modified_at',
      width: 170,
      render: (t: string | null) =>
        t ? new Date(t).toLocaleString('zh-CN') : '-',
    },
  ]

  const normalizedPath = currentPath.endsWith('/') && currentPath !== '/'
    ? currentPath.slice(0, -1)
    : currentPath

  const parentPath =
    normalizedPath !== '/'
      ? normalizedPath.substring(0, normalizedPath.lastIndexOf('/')) || '/'
      : null

  return (
    <div>
      <Space style={{ marginBottom: 8 }}>
        {parentPath !== null && (
          <Button
            size="small"
            icon={<ArrowLeftOutlined />}
            onClick={() => onNavigate(parentPath)}
          >
            上级目录
          </Button>
        )}
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {currentPath}
        </Typography.Text>
      </Space>

      {entries.length === 0 ? (
        <Typography.Text type="secondary">目录为空</Typography.Text>
      ) : (
        <Table<DirEntry>
          dataSource={entries}
          columns={columns}
          rowKey="path"
          size="small"
          pagination={false}
        />
      )}
    </div>
  )
}

export default DirectoryTree
