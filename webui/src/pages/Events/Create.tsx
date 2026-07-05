import { useState } from 'react'
import {
  Button,
  Card,
  Form,
  Input,
  Select,
  Space,
  Switch,
  Typography,
  message,
} from 'antd'
import { useNavigate } from 'react-router-dom'
import { createEventRule } from '../../services/events'

const { Title } = Typography
const { TextArea } = Input

// Must match the DB event_type enum exactly (underscore form). The previous
// values (dotted names, file.indexed, agent.registered) were invalid and, in
// particular, file_deleted was missing so those rules could never be created.
const EVENT_TYPE_OPTIONS = [
  { label: 'file_uploaded', value: 'file_uploaded' },
  { label: 'file_deleted', value: 'file_deleted' },
  { label: 'agent_online', value: 'agent_online' },
  { label: 'agent_offline', value: 'agent_offline' },
  { label: 'agent_approved', value: 'agent_approved' },
  { label: 'agent_revoked', value: 'agent_revoked' },
]

// Action types wired end-to-end by the Control Plane event engine (CC-7):
//   - webhook: POSTs the event payload to a configured URL (with retries).
//   - nats_publish: re-publishes the payload to a configured NATS subject.
// kafka_publish exists in the DB enum but has no implementation (no Kafka in the
// deployment) and is rejected by the API, so it is intentionally not offered.
const ACTION_TYPE_OPTIONS = [
  { label: 'webhook', value: 'webhook' },
  { label: 'nats_publish', value: 'nats_publish' },
]

/**
 * Create event rule page — form for name, event type, action type,
 * action config JSON, optional filter JSON, and enabled flag.
 */
function EventCreatePage() {
  const navigate = useNavigate()
  const [form] = Form.useForm()
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async (values: {
    name: string
    event_type: string
    action_type: string
    action_config_raw: string
    filter_raw?: string
    enabled: boolean
  }) => {
    let actionConfig: Record<string, unknown>
    let filter: Record<string, unknown> = {}

    try {
      actionConfig = JSON.parse(values.action_config_raw)
    } catch {
      message.error('动作配置不是合法的 JSON')
      return
    }

    if (values.filter_raw) {
      try {
        filter = JSON.parse(values.filter_raw)
      } catch {
        message.error('过滤条件不是合法的 JSON')
        return
      }
    }

    setSubmitting(true)
    try {
      await createEventRule({
        name: values.name,
        event_type: values.event_type,
        action_type: values.action_type,
        action_config: actionConfig,
        filter,
        enabled: values.enabled,
      })
      message.success('事件规则创建成功')
      navigate('/events')
    } catch {
      message.error('创建失败，请重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button onClick={() => navigate('/events')}>← 返回列表</Button>
        <Title level={3} style={{ margin: 0 }}>新建事件规则</Title>
      </Space>

      <Card style={{ maxWidth: 680 }}>
        <Form
          form={form}
          layout="vertical"
          initialValues={{ enabled: true, action_config_raw: '{}', filter_raw: '{}' }}
          onFinish={handleSubmit}
        >
          <Form.Item
            label="规则名称"
            name="name"
            rules={[{ required: true, message: '请输入规则名称' }]}
          >
            <Input placeholder="例如：文件上传通知" maxLength={64} />
          </Form.Item>

          <Form.Item
            label="事件类型"
            name="event_type"
            rules={[{ required: true, message: '请选择事件类型' }]}
          >
            <Select options={EVENT_TYPE_OPTIONS} placeholder="选择事件类型" />
          </Form.Item>

          <Form.Item
            label="动作类型"
            name="action_type"
            rules={[{ required: true, message: '请选择动作类型' }]}
          >
            <Select options={ACTION_TYPE_OPTIONS} placeholder="选择动作类型" />
          </Form.Item>

          <Form.Item
            label="动作配置（JSON）"
            name="action_config_raw"
            rules={[{ required: true, message: '请填写动作配置' }]}
            extra='webhook：{"url": "https://example.com/hook"}；nats_publish：{"subject": "events.custom.sink"}'
          >
            <TextArea rows={4} placeholder='{"url": "https://example.com/hook"}' />
          </Form.Item>

          <Form.Item
            label="过滤条件（JSON，可选）"
            name="filter_raw"
            extra="留空或 {} 表示不过滤"
          >
            <TextArea rows={3} placeholder="{}" />
          </Form.Item>

          <Form.Item label="启用" name="enabled" valuePropName="checked">
            <Switch />
          </Form.Item>

          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit" loading={submitting}>
                创建
              </Button>
              <Button onClick={() => navigate('/events')}>取消</Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}

export default EventCreatePage
