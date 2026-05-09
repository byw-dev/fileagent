import { useState, useEffect } from 'react'
import {
  App,
  Button,
  Card,
  Descriptions,
  Form,
  Input,
  Modal,
  Space,
  Spin,
  Typography,
} from 'antd'
import { EditOutlined, DeleteOutlined } from '@ant-design/icons'
import { useParams, useNavigate } from 'react-router-dom'
import {
  getFileType,
  updateFileType,
  deleteFileType,
} from '../../services/file-types'
import type { FileType } from '../../services/file-types'

const { Title } = Typography
const { TextArea } = Input

/**
 * File type detail page — shows metadata, supports inline edit and delete.
 */
function FileTypeDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { modal, message } = App.useApp()

  const [fileType, setFileType] = useState<FileType | null>(null)
  const [loading, setLoading] = useState(true)
  const [editOpen, setEditOpen] = useState(false)
  const [saving, setSaving] = useState(false)
  const [form] = Form.useForm()

  useEffect(() => {
    if (!id) return
    setLoading(true)
    getFileType(id)
      .then(setFileType)
      .catch(() => message.error('获取文件类型信息失败'))
      .finally(() => setLoading(false))
  }, [id])

  const openEdit = () => {
    if (!fileType) return
    form.setFieldsValue({ name: fileType.name, description: fileType.description ?? '' })
    setEditOpen(true)
  }

  const handleSave = async (values: { name: string; description?: string }) => {
    if (!id) return
    setSaving(true)
    try {
      const updated = await updateFileType(id, values)
      setFileType(updated)
      setEditOpen(false)
      message.success('更新成功')
    } catch {
      message.error('更新失败')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = () => {
    if (!fileType || !id) return
    modal.confirm({
      title: `删除文件类型：${fileType.name}`,
      content: '确认删除该文件类型？',
      okType: 'danger',
      onOk: async () => {
        try {
          await deleteFileType(id)
          message.success('删除成功')
          navigate('/file-types')
        } catch {
          message.error('删除失败')
        }
      },
    })
  }

  if (loading) {
    return (
      <Spin size="large" style={{ display: 'block', textAlign: 'center', marginTop: 80 }} />
    )
  }

  if (!fileType) {
    return <Typography.Text type="danger">文件类型不存在或加载失败</Typography.Text>
  }

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }} wrap>
        <Space>
          <Button onClick={() => navigate('/file-types')}>← 返回列表</Button>
          <Title level={4} style={{ margin: 0 }}>{fileType.name}</Title>
        </Space>
        <Space>
          <Button icon={<EditOutlined />} onClick={openEdit}>编辑</Button>
          <Button danger icon={<DeleteOutlined />} onClick={handleDelete}>删除</Button>
        </Space>
      </Space>

      <Card>
        <Descriptions column={2} bordered>
          <Descriptions.Item label="ID">{fileType.id}</Descriptions.Item>
          <Descriptions.Item label="名称">{fileType.name}</Descriptions.Item>
          <Descriptions.Item label="描述" span={2}>
            {fileType.description ?? '（无描述）'}
          </Descriptions.Item>
          <Descriptions.Item label="创建时间">
            {new Date(fileType.created_at).toLocaleString('zh-CN')}
          </Descriptions.Item>
        </Descriptions>
      </Card>

      <Modal
        title="编辑文件类型"
        open={editOpen}
        onCancel={() => setEditOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={saving}
        okText="保存"
        cancelText="取消"
      >
        <Form form={form} layout="vertical" onFinish={handleSave}>
          <Form.Item
            label="名称"
            name="name"
            rules={[{ required: true, message: '请输入名称' }]}
          >
            <Input maxLength={64} />
          </Form.Item>
          <Form.Item label="描述" name="description">
            <TextArea rows={3} maxLength={256} showCount />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

export default FileTypeDetailPage
