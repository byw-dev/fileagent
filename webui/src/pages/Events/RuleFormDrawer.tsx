import { useState, useEffect } from 'react'
import { App, Form, Input, Select, Switch } from 'antd'
import FormDrawer from '../../components/FormDrawer'
import { createEventRule, updateEventRule } from '../../services/events'
import type { EventRule } from '../../services/events'

const { TextArea } = Input

// Must match the DB event_type enum exactly (underscore form).
const EVENT_TYPE_OPTIONS = [
  { label: 'file_uploaded', value: 'file_uploaded' },
  { label: 'file_deleted', value: 'file_deleted' },
  { label: 'agent_online', value: 'agent_online' },
  { label: 'agent_offline', value: 'agent_offline' },
  { label: 'agent_approved', value: 'agent_approved' },
  { label: 'agent_revoked', value: 'agent_revoked' },
]

// Only webhook / nats_publish are wired end-to-end by the CP event engine (CC-7 /
// D-019); kafka_publish exists in the enum but has no implementation.
const ACTION_TYPE_OPTIONS = [
  { label: 'webhook', value: 'webhook' },
  { label: 'nats_publish', value: 'nats_publish' },
]

interface RuleFormValues {
  name: string
  event_type: string
  action_type: string
  url?: string
  subject?: string
  filter_raw?: string
  enabled: boolean
}

interface RuleFormDrawerProps {
  open: boolean
  /** Rule being edited, or null for create. */
  editing: EventRule | null
  onClose: () => void
  /** Called after a successful create/update so the list can reload. */
  onSaved: () => void
}

/** Pretty-print a filter object back into the textarea, or '' for empty. */
function filterToRaw(filter: Record<string, unknown> | undefined): string {
  if (!filter || Object.keys(filter).length === 0) return ''
  return JSON.stringify(filter, null, 2)
}

/**
 * RuleFormDrawer — create/edit an event rule in the site-wide 480px drawer
 * (交互定则 1). The action config is structured, not raw JSON: choosing an action
 * swaps the required field (webhook→url / nats_publish→subject, 4b 联动必填).
 */
function RuleFormDrawer({ open, editing, onClose, onSaved }: RuleFormDrawerProps) {
  const { message } = App.useApp()
  const [form] = Form.useForm<RuleFormValues>()
  const [submitting, setSubmitting] = useState(false)

  // Reset fields whenever the drawer (re)opens so create/edit start clean and
  // edit backfills from the selected rule.
  useEffect(() => {
    if (!open) return
    if (editing) {
      const cfg = editing.action_config ?? {}
      form.setFieldsValue({
        name: editing.name,
        event_type: editing.event_type,
        action_type: editing.action_type,
        url: typeof cfg.url === 'string' ? cfg.url : undefined,
        subject: typeof cfg.subject === 'string' ? cfg.subject : undefined,
        filter_raw: filterToRaw(editing.filter),
        enabled: editing.enabled,
      })
    } else {
      form.resetFields()
      form.setFieldsValue({ enabled: true, action_type: 'webhook' })
    }
  }, [open, editing, form])

  const submit = async () => {
    let values: RuleFormValues
    try {
      values = await form.validateFields()
    } catch {
      return
    }

    let filter: Record<string, unknown> = {}
    if (values.filter_raw && values.filter_raw.trim()) {
      try {
        filter = JSON.parse(values.filter_raw)
      } catch {
        message.error('过滤条件不是合法的 JSON')
        return
      }
    }

    const action_config: Record<string, unknown> =
      values.action_type === 'webhook'
        ? { url: values.url }
        : { subject: values.subject }

    const payload = {
      name: values.name,
      event_type: values.event_type,
      action_type: values.action_type,
      action_config,
      filter,
      enabled: values.enabled,
    }

    setSubmitting(true)
    try {
      if (editing) {
        await updateEventRule(editing.id, payload)
        message.success('规则已更新')
      } else {
        await createEventRule(payload)
        message.success('规则已创建')
      }
      onSaved()
    } catch {
      message.error('保存失败，请重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <FormDrawer
      open={open}
      title={editing ? `编辑规则：${editing.name}` : '新建事件规则'}
      onClose={onClose}
      onSubmit={submit}
      loading={submitting}
    >
      <Form form={form} layout="vertical" preserve={false} onFinish={submit}>
        <Form.Item
          name="name"
          label="规则名称"
          rules={[{ required: true, whitespace: true, message: '请输入规则名称' }]}
        >
          <Input placeholder="例如：文件上传通知" maxLength={64} />
        </Form.Item>

        <Form.Item
          name="event_type"
          label="事件类型"
          rules={[{ required: true, message: '请选择事件类型' }]}
        >
          <Select options={EVENT_TYPE_OPTIONS} placeholder="选择事件类型" />
        </Form.Item>

        <Form.Item
          name="action_type"
          label="动作类型"
          rules={[{ required: true, message: '请选择动作类型' }]}
        >
          <Select options={ACTION_TYPE_OPTIONS} placeholder="选择动作类型" />
        </Form.Item>

        {/* 4b: the required config field follows the chosen action. */}
        <Form.Item noStyle shouldUpdate={(a, b) => a.action_type !== b.action_type}>
          {({ getFieldValue }) =>
            getFieldValue('action_type') === 'nats_publish' ? (
              <Form.Item
                name="subject"
                label="NATS Subject"
                rules={[{ required: true, whitespace: true, message: '请输入 NATS subject' }]}
              >
                <Input placeholder="例如：events.custom.sink" />
              </Form.Item>
            ) : (
              <Form.Item
                name="url"
                label="Webhook URL"
                rules={[
                  { required: true, whitespace: true, message: '请输入 Webhook URL' },
                  { type: 'url', message: '请输入合法的 URL' },
                ]}
              >
                <Input placeholder="https://example.com/hook" />
              </Form.Item>
            )
          }
        </Form.Item>

        <Form.Item name="filter_raw" label="过滤条件（JSON，可选）" extra="留空表示不过滤">
          <TextArea rows={3} placeholder="{}" />
        </Form.Item>

        <Form.Item name="enabled" label="启用" valuePropName="checked">
          <Switch />
        </Form.Item>
      </Form>
    </FormDrawer>
  )
}

export default RuleFormDrawer
