import { useState, useEffect } from 'react'
import {
  Button,
  Card,
  Form,
  Input,
  Modal,
  Space,
  Table,
  Typography,
  message,
} from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { listBuckets, createBucket } from '../../services/buckets'
import type { Bucket } from '../../services/buckets'
import useAuthStore from '../../store/auth'

const { Title } = Typography
const { TextArea } = Input

/**
 * Buckets page — shows all storage buckets; super_admin can create new ones.
 */
function BucketsPage() {
  const user = useAuthStore((s) => s.user)
  const isSuperAdmin = user?.role === 'super_admin'

  const [buckets, setBuckets] = useState<Bucket[]>([])
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [form] = Form.useForm()

  const loadBuckets = () => {
    setLoading(true)
    listBuckets()
      .then(setBuckets)
      .catch(() => message.error('获取 Bucket 列表失败'))
      .finally(() => setLoading(false))
  }

  useEffect(loadBuckets, [])

  const handleCreate = async (values: { name: string; description?: string }) => {
    setSubmitting(true)
    try {
      await createBucket(values)
      message.success('Bucket 创建成功')
      setCreateOpen(false)
      form.resetFields()
      loadBuckets()
    } catch {
      message.error('创建失败')
    } finally {
      setSubmitting(false)
    }
  }

  const columns: ColumnsType<Bucket> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (name: string) => <Typography.Text strong>{name}</Typography.Text>,
    },
    {
      title: '描述',
      dataIndex: 'description',
      key: 'description',
      ellipsis: true,
      render: (v: string | null) => v ?? '—',
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (v: string) => new Date(v).toLocaleString('zh-CN'),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>Bucket 管理</Title>
        {isSuperAdmin && (
          <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>
            新建 Bucket
          </Button>
        )}
      </Space>

      <Card>
        <Table
          dataSource={buckets}
          columns={columns}
          rowKey="id"
          loading={loading}
          pagination={{ pageSize: 20 }}
          locale={{ emptyText: '暂无 Bucket' }}
        />
      </Card>

      <Modal
        title="新建 Bucket"
        open={createOpen}
        onCancel={() => { setCreateOpen(false); form.resetFields() }}
        onOk={() => form.submit()}
        confirmLoading={submitting}
        okText="创建"
        cancelText="取消"
      >
        <Form form={form} layout="vertical" onFinish={handleCreate}>
          <Form.Item
            label="名称"
            name="name"
            rules={[{ required: true, message: '请输入 Bucket 名称' }]}
          >
            <Input placeholder="例如：logs-bucket" maxLength={63} />
          </Form.Item>
          <Form.Item label="描述" name="description">
            <TextArea rows={2} maxLength={256} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

export default BucketsPage
