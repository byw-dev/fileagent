import { useRef, useState } from 'react'
import { App, Button, Space, Typography, Tag, Switch } from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { listEventRules, deleteEventRule, updateEventRule } from '../../services/events'
import type { EventRule } from '../../services/events'
import { DangerConfirmModal } from '../../components/DangerConfirm'
import TimeText from '../../components/TimeText'
import RuleFormDrawer from './RuleFormDrawer'
import DeliveriesDrawer from './DeliveriesDrawer'

const { Title } = Typography

/**
 * Event rules list page (WR-5). Create/edit run in a 480px drawer (交互定则 1),
 * delivery history opens in a drawer (4c), the enable switch toggles inline
 * (4a), and delete uses the danger-confirm modal (交互定则 2).
 */
function EventsPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  const { message } = App.useApp()

  const [togglingId, setTogglingId] = useState<string | null>(null)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editing, setEditing] = useState<EventRule | null>(null)
  const [deliveriesFor, setDeliveriesFor] = useState<EventRule | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<EventRule | null>(null)
  const [deleting, setDeleting] = useState(false)

  const openCreate = () => {
    setEditing(null)
    setDrawerOpen(true)
  }
  const openEdit = (rule: EventRule) => {
    setEditing(rule)
    setDrawerOpen(true)
  }

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
      message.success(enabled ? '规则已启用' : '规则已停用')
      actionRef.current?.reload()
    } catch {
      message.error('操作失败')
    } finally {
      setTogglingId(null)
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await deleteEventRule(deleteTarget.id)
      message.success('已删除')
      setDeleteTarget(null)
      actionRef.current?.reload()
    } catch {
      message.error('删除失败')
    } finally {
      setDeleting(false)
    }
  }

  const columns: ProColumns<EventRule>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      ellipsis: true,
      render: (_, rule) => (
        <Button type="link" style={{ padding: 0, height: 'auto' }} onClick={() => openEdit(rule)}>
          {rule.name}
        </Button>
      ),
    },
    {
      title: '事件类型',
      dataIndex: 'event_type',
      key: 'event_type',
      width: 160,
      render: (_, rule) => <Tag>{rule.event_type}</Tag>,
    },
    {
      title: '动作类型',
      dataIndex: 'action_type',
      key: 'action_type',
      width: 140,
      render: (_, rule) => <Tag>{rule.action_type}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 80,
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
      width: 150,
      render: (_, rule) => <TimeText value={rule.created_at} />,
    },
    {
      title: '操作',
      key: 'action',
      width: 140,
      align: 'right',
      render: (_, rule) => (
        <Space size="middle">
          <Button
            type="link"
            style={{ padding: 0, height: 'auto' }}
            onClick={() => setDeliveriesFor(rule)}
          >
            投递记录
          </Button>
          <Button
            type="link"
            danger
            style={{ padding: 0, height: 'auto' }}
            onClick={() => setDeleteTarget(rule)}
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
        <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
          新建规则
        </Button>
      </Space>

      <ProTable<EventRule>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        request={async () => {
          try {
            const data = await listEventRules()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      <RuleFormDrawer
        open={drawerOpen}
        editing={editing}
        onClose={() => setDrawerOpen(false)}
        onSaved={() => {
          setDrawerOpen(false)
          actionRef.current?.reload()
        }}
      />

      <DeliveriesDrawer rule={deliveriesFor} onClose={() => setDeliveriesFor(null)} />

      <DangerConfirmModal
        open={deleteTarget !== null}
        title={deleteTarget ? `删除事件规则：${deleteTarget.name}` : '删除事件规则'}
        content="删除后该规则不再触发，相关投递记录保留。此操作不可撤销。"
        loading={deleting}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={confirmDelete}
      />
    </div>
  )
}

export default EventsPage
