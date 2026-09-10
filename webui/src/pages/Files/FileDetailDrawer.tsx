import { useState } from 'react'
import { App, Button, Drawer, Descriptions, Space, Tag, Typography } from 'antd'
import { DownloadOutlined, LinkOutlined } from '@ant-design/icons'
import useSWR from 'swr'
import { getFile, getFileDownloadUrl } from '../../services/files'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'

const { Text } = Typography

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

interface FileDetailDrawerProps {
  /** File id to show; null closes the drawer. */
  fileId: string | null
  onClose: () => void
}

/**
 * FileDetailDrawer — file metadata + presigned download in the site-wide 480px
 * right drawer (3a, 交互定则 1). Replaces the former full-page /files/:id route.
 */
function FileDetailDrawer({ fileId, onClose }: FileDetailDrawerProps) {
  const { message } = App.useApp()
  const { data: file, isLoading } = useSWR(
    fileId ? ['file', fileId] : null,
    () => getFile(fileId!),
    { onError: () => message.error('获取文件信息失败') },
  )
  const [busy, setBusy] = useState(false)

  const download = async () => {
    if (!file) return
    setBusy(true)
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
    } finally {
      setBusy(false)
    }
  }

  const copyLink = async () => {
    if (!file) return
    // Separate the two failure modes so a clipboard error (permission / insecure
    // context) isn't misreported as a link-fetch failure.
    let url: string
    try {
      ;({ url } = await getFileDownloadUrl(file.id))
    } catch {
      message.error('获取下载链接失败')
      return
    }
    try {
      await navigator.clipboard.writeText(url)
      message.success('下载链接已复制')
    } catch {
      // The URL isn't shown in the drawer, so don't tell the user to copy it
      // manually — report the actual cause (clipboard blocked / insecure context).
      message.error('复制失败：浏览器剪贴板不可用，请改用「下载」')
    }
  }

  const tags = file?.tags ?? {}
  const tagKeys = Object.keys(tags).sort()

  return (
    <Drawer
      open={fileId !== null}
      title={file?.filename ?? '文件详情'}
      onClose={onClose}
      width={480}
      destroyOnHidden
      loading={isLoading}
      extra={
        <Space>
          <Button icon={<LinkOutlined />} onClick={copyLink} disabled={!file}>
            复制链接
          </Button>
          <Button type="primary" icon={<DownloadOutlined />} loading={busy} onClick={download} disabled={!file}>
            下载
          </Button>
        </Space>
      }
    >
      {file && (
        <Descriptions column={1} size="small" bordered>
          {file.meta_incomplete && <Descriptions.Item label="元数据"><Tag color="warning">元数据不完整</Tag></Descriptions.Item>}
          <Descriptions.Item label="状态">
            <StatusBadge status={file.status} domain="file" />
          </Descriptions.Item>
          <Descriptions.Item label="标签">
            {tagKeys.length === 0 ? (
              <Text type="secondary">—</Text>
            ) : (
              <Space size={[4, 4]} wrap>
                {tagKeys.map((k) => (
                  <Tag key={k} style={{ margin: 0 }}>
                    {k}:{tags[k]}
                  </Tag>
                ))}
              </Space>
            )}
          </Descriptions.Item>
          <Descriptions.Item label="大小">{formatBytes(file.size)}</Descriptions.Item>
          <Descriptions.Item label="MIME 类型">{file.mime_type || '—'}</Descriptions.Item>
          <Descriptions.Item label="原始路径">{file.original_path || '—'}</Descriptions.Item>
          <Descriptions.Item label="存储路径">{file.storage_key}</Descriptions.Item>
          <Descriptions.Item label="SHA-256">
            <Text code style={{ fontSize: 12, wordBreak: 'break-all' }}>{file.sha256 || '—'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label="Bucket ID">{file.bucket_id}</Descriptions.Item>
          <Descriptions.Item label="文件类型 ID">{file.file_type_id ?? '—'}</Descriptions.Item>
          <Descriptions.Item label="上传时间"><TimeText value={file.uploaded_at} /></Descriptions.Item>
          <Descriptions.Item label="索引时间"><TimeText value={file.indexed_at} /></Descriptions.Item>
        </Descriptions>
      )}
    </Drawer>
  )
}

export default FileDetailDrawer
