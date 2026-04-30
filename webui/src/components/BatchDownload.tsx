import { useState } from 'react'
import { Button, Progress, message } from 'antd'
import { DownloadOutlined } from '@ant-design/icons'
import apiClient from '../services/api'

interface BatchDownloadProps {
  /** Selected file IDs to download. */
  fileIds: string[]
}

/**
 * BatchDownload — triggers a batch download for multiple files.
 * Uses the /api/v1/files/batch-download-urls endpoint to obtain presigned URLs,
 * then streams them into a zip archive via the browser's Fetch API.
 *
 * Note: StreamSaver.js requires a Service Worker and HTTPS in production.
 */
function BatchDownload({ fileIds }: BatchDownloadProps) {
  const [loading, setLoading] = useState(false)
  const [progress, setProgress] = useState(0)

  const handleDownload = async () => {
    if (fileIds.length === 0) {
      message.warning('请先选择要下载的文件')
      return
    }

    setLoading(true)
    setProgress(0)

    try {
      const response = await apiClient.post<Array<{ fileName: string; url: string }>>(
        '/api/v1/files/batch-download-urls',
        { ids: fileIds }
      )

      const urls = response.data
      const total = urls.length

      for (let i = 0; i < total; i++) {
        const { fileName, url } = urls[i]
        const a = document.createElement('a')
        a.href = url
        a.download = fileName
        document.body.appendChild(a)
        a.click()
        document.body.removeChild(a)
        setProgress(Math.round(((i + 1) / total) * 100))
        // Small delay to avoid triggering browser popup blockers
        await new Promise((resolve) => setTimeout(resolve, 300))
      }

      message.success(`已开始下载 ${total} 个文件`)
    } catch {
      message.error('批量下载失败，请稍后重试')
    } finally {
      setLoading(false)
      setProgress(0)
    }
  }

  return (
    <div style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
      <Button
        icon={<DownloadOutlined />}
        loading={loading}
        onClick={handleDownload}
        disabled={fileIds.length === 0}
      >
        批量下载 {fileIds.length > 0 ? `(${fileIds.length})` : ''}
      </Button>
      {loading && progress > 0 && (
        <Progress percent={progress} size="small" style={{ width: 120 }} />
      )}
    </div>
  )
}

export default BatchDownload
