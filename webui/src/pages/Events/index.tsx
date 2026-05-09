import { useState } from 'react'
import { App, Button, Space, Typography, Tag, Switch } from 'antd'
import { PlusOutlined, DeleteOutlined, UnorderedListOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useNavigate } from 'react-router-dom'
import { useRef } from 'react'
import { listEventRules, deleteEventRule, updateEventRule } from '../../services/events'
import type { EventRule } from '../../services/events'

const { Title } = Typography

/**
 * Event rules list page — shows all event rules with create, toggle and delete actions.
 */
function EventsPage() {
  const navigate = useNavigate()
  const actionRef = useRef<ActionType | undefined>(undefined)
  const [togglingId, setTogglingId] = useState<string | null>(null)
  const { modal, message } = App.useApp()

  const handleToggle = async (rule: EventRule, enabled: boolean) => {
    setTogglingId(rule.id)
    try {
      await updateEventRule(rule.id, {
        name: rule.name,
        event_type: rule.event_type,
        filter: rule.filter,
        action_type: rule.action_type,
        action_config: rule.action_config,
        enabled,
      })
      message.success(enabled ? '规则已启用' : '规则已禁用')
      actionRef.current?.reload()
    } catch {
      message.error('操作失败')
    } finally {
      setTogglingId(null)
    }
  }

  const handleDelete = (rule: EventRule) => {
    modal.confirm({
      title: `删除事件规则：${rule.name}`,
      content: '确认删除该规则？相关投递记录将不再触发。',
      okType: 'danger',
      onOk: async () => {
        try {
          await deleteEventRule(rule.id)
          message.success('删除成功')
          actionRef.current?.reload()
        } catch {
          message.error('删除失败')
        }
      },
    })
  }

  const columns: ProColumns<EventRule>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      ellipsis: true,
    },
    {
      title: '事件类型',
      dataIndex: 'event_type',
      key: 'event_type',
      width: 160,
      render: (_, rule) => <Tag color="blue">{rule.event_type}</Tag>,
    },
    {
      title: '动作类型',
      dataIndex: 'action_type',
      key: 'action_type',
      width: 140,
      render: (_, rule) => <Tag color="purple">{rule.action_type}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 90,
      render: (_, rule) => (
        <Switch
          checked={rule.enabled}
          size="small"
          loading={togglingId === rule.id}
          onChange={(checked) => handleToggle(rule, checked)}
        />
      ),
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (_, rule) => new Date(rule.created_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 170,
      render: (_, rule) => (
        <Space>
          <Button
            size="small"
            icon={<UnorderedListOutlined />}
            onClick={() => navigate(`/events/${rule.id}/deliveries`)}
          >
            投递记录
          </Button>
          <Button
            size="small"
            danger
            icon={<DeleteOutlined />}
            onClick={() => handleDelete(rule)}
          >
            删除
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>事件规则</Title>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => navigate('/events/create')}
        >
          新建规则
        </Button>
      </Space>

      <ProTable<EventRule>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        request={async () => {
          try {
            const data = await listEventRules()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />
    </div>
  )
}

export default EventsPage
