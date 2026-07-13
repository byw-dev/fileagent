import { useState, useEffect } from 'react'
import {
  Button,
  Card,
  Descriptions,
  Modal,
  Space,
  Spin,
  Tag,
  Typography,
  message,
} from 'antd'
import { DownloadOutlined, CopyOutlined } from '@ant-design/icons'
import { useParams, useNavigate } from 'react-router-dom'
import { getFile, getFileDownloadUrl, FILE_STATUS_COLOR } from '../../services/files'
import type { FileEntry } from '../../services/files'

const { Title, Text } = Typography

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * File detail page — displays file metadata and provides a presigned download link.
 */
function FileDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()

  const [file, setFile] = useState<FileEntry | null>(null)
  const [loading, setLoading] = useState(true)
  const [downloading, setDownloading] = useState(false)
  const [urlModalOpen, setUrlModalOpen] = useState(false)
  const [downloadUrl, setDownloadUrl] = useState('')

  useEffect(() => {
    if (!id) return
    setLoading(true)
    getFile(id)
      .then(setFile)
      .catch(() => message.error('获取文件信息失败'))
      .finally(() => setLoading(false))
  }, [id])

  const handleGetUrl = async () => {
    if (!id) return
    setDownloading(true)
    try {
      const { url } = await getFileDownloadUrl(id)
      setDownloadUrl(url)
      setUrlModalOpen(true)
    } catch {
      message.error('获取下载链接失败')
    } finally {
      setDownloading(false)
    }
  }

  const handleDownload = () => {
    if (!file || !downloadUrl) return
    const a = document.createElement('a')
    a.href = downloadUrl
    a.download = file.filename
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  }

  const handleCopyUrl = () => {
    navigator.clipboard.writeText(downloadUrl).then(() => message.success('链接已复制'))
  }

  if (loading) {
    return (
      <Spin size="large" style={{ display: 'block', textAlign: 'center', marginTop: 80 }} />
    )
  }

  if (!file) {
    return <Text type="danger">文件不存在或加载失败</Text>
  }

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }} wrap>
        <Space>
          <Button onClick={() => navigate('/files')}>← 返回列表</Button>
          <Title level={4} style={{ margin: 0 }}>{file.filename}</Title>
          <Tag color={FILE_STATUS_COLOR[file.status] ?? 'default'}>{file.status}</Tag>
        </Space>
        <Button
          type="primary"
          icon={<DownloadOutlined />}
          loading={downloading}
          onClick={handleGetUrl}
        >
          获取下载链接
        </Button>
      </Space>

      <Card>
        <Descriptions column={2} bordered>
          <Descriptions.Item label="ID">{file.id}</Descriptions.Item>
          <Descriptions.Item label="文件名">{file.filename}</Descriptions.Item>
          <Descriptions.Item label="原始路径" span={2}>
            {file.original_path || '—'}
          </Descriptions.Item>
          <Descriptions.Item label="存储路径" span={2}>
            {file.storage_key}
          </Descriptions.Item>
          <Descriptions.Item label="大小">{formatBytes(file.size)}</Descriptions.Item>
          <Descriptions.Item label="MIME 类型">{file.mime_type || '—'}</Descriptions.Item>
          <Descriptions.Item label="SHA-256" span={2}>
            <Text code style={{ fontSize: 12 }}>{file.sha256 || '—'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label="Bucket ID">{file.bucket_id}</Descriptions.Item>
          <Descriptions.Item label="文件类型 ID">
            {file.file_type_id ?? '—'}
          </Descriptions.Item>
          <Descriptions.Item label="上传时间">
            {new Date(file.uploaded_at).toLocaleString('zh-CN')}
          </Descriptions.Item>
          <Descriptions.Item label="索引时间">
            {file.indexed_at ? new Date(file.indexed_at).toLocaleString('zh-CN') : '—'}
          </Descriptions.Item>
        </Descriptions>
      </Card>

      <Modal
        title="预签名下载链接"
        open={urlModalOpen}
        onCancel={() => setUrlModalOpen(false)}
        footer={
          <Space>
            <Button icon={<CopyOutlined />} onClick={handleCopyUrl}>复制链接</Button>
            <Button type="primary" icon={<DownloadOutlined />} onClick={handleDownload}>
              立即下载
            </Button>
          </Space>
        }
        width={640}
      >
        <Text
          copyable
          style={{ wordBreak: 'break-all', fontSize: 12 }}
        >
          {downloadUrl}
        </Text>
      </Modal>
    </div>
  )
}

export default FileDetailPage
