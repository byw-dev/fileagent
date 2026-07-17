import { useRef, useState } from 'react'
import { App, Button, Form, Input, Space, Typography } from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { listBuckets, createBucket } from '../../services/buckets'
import type { Bucket } from '../../services/buckets'
import FormDrawer from '../../components/FormDrawer'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'
import useAuthStore from '../../store/auth'

const { Title, Text } = Typography
const { TextArea } = Input

/** S3-compatible bucket name pattern: 3-63 chars, lowercase, digits, hyphens only,
 *  must not start or end with a hyphen. */
const BUCKET_NAME_RE = /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/

interface BucketFormValues {
  name: string
  description?: string
}

/**
 * Buckets page (WR-7) — a registration view (登记视图) of storage buckets. The
 * list is read-only; only super_admin sees the 新建 action (4d), which opens the
 * site-wide 480px drawer (交互定则 1). MM-DD HH:mm times (定则 5), one-line empty
 * state (定则 6).
 *
 * Note: per-bucket event-notification config is a deployment-level MinIO concern
 * (notify_webhook, init-minio.sh) and is not tracked per bucket by the Control
 * Plane, so the 4d「通知配置状态」sub-item is descoped (no backend data).
 */
function BucketsPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  const user = useAuthStore((s) => s.user)
  const isSuperAdmin = user?.role === 'super_admin'
  const { message } = App.useApp()

  const [drawerOpen, setDrawerOpen] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [form] = Form.useForm<BucketFormValues>()

  const submit = async () => {
    let values: BucketFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }
    setSubmitting(true)
    try {
      await createBucket(values)
      message.success('Bucket 创建成功')
      setDrawerOpen(false)
      actionRef.current?.reload()
    } catch {
      message.error('创建失败，请检查 Bucket 名称或稍后重试')
    } finally {
      setSubmitting(false)
    }
  }

  const columns: ProColumns<Bucket>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (_, b) => <Text strong>{b.name}</Text>,
    },
    {
      title: '描述',
      dataIndex: 'description',
      key: 'description',
      ellipsis: true,
      render: (_, b) => b.description || <Text type="secondary">—</Text>,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 160,
      render: (_, b) => <TimeText value={b.created_at} />,
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>Bucket 管理</Title>
        {isSuperAdmin && (
          <Button type="primary" icon={<PlusOutlined />} onClick={() => setDrawerOpen(true)}>
            新建 Bucket
          </Button>
        )}
      </Space>

      <ProTable<Bucket>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        locale={{
          emptyText: (
            <EmptyState
              description="还没有 Bucket"
              action={
                isSuperAdmin ? (
                  <Button type="primary" icon={<PlusOutlined />} onClick={() => setDrawerOpen(true)}>
                    新建 Bucket
                  </Button>
                ) : undefined
              }
            />
          ),
        }}
        request={async () => {
          try {
            const data = await listBuckets()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      <FormDrawer
        open={drawerOpen}
        title="新建 Bucket"
        onClose={() => setDrawerOpen(false)}
        onSubmit={submit}
        loading={submitting}
        submitText="创建"
      >
        <Form form={form} layout="vertical" preserve={false} onFinish={submit}>
          <Form.Item
            label="名称"
            name="name"
            rules={[
              { required: true, message: '请输入 Bucket 名称' },
              {
                validator: (_, value) => {
                  if (!value || BUCKET_NAME_RE.test(value)) return Promise.resolve()
                  return Promise.reject(
                    new Error('只能包含小写字母、数字和连字符，长度 3-63，不能以连字符开头或结尾'),
                  )
                },
              },
            ]}
          >
            <Input placeholder="例如：logs-bucket" maxLength={63} />
          </Form.Item>
          <Form.Item label="描述" name="description">
            <TextArea placeholder="可选描述" rows={2} maxLength={256} showCount />
          </Form.Item>
        </Form>
      </FormDrawer>
    </div>
  )
}

export default BucketsPage
