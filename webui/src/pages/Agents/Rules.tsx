import { useState, useEffect } from 'react'
import { App, Button, Space, Table, Tag, Typography, Switch } from 'antd'
import { PlusOutlined, DeleteOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useParams, useNavigate } from 'react-router-dom'
import { listRules, deleteRule } from '../../services/agents'
import type { CollectionRule } from '../../services/agents'

const { Title } = Typography

/**
 * Agent rules standalone page — shows collection rules for a specific agent
 * with create and delete actions.
 */
function AgentRulesPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { modal, message } = App.useApp()

  const [rules, setRules] = useState<CollectionRule[]>([])
  const [loading, setLoading] = useState(true)

  const loadRules = () => {
    if (!id) return
    setLoading(true)
    listRules(id)
      .then((data) => setRules(data.items))
      .catch(() => message.error('获取规则列表失败'))
      .finally(() => setLoading(false))
  }

  useEffect(loadRules, [id])

  const handleDelete = (rule: CollectionRule) => {
    if (!id) return
    modal.confirm({
      title: `删除规则：${rule.name}`,
      content: '确认删除该采集规则？',
      okType: 'danger',
      onOk: async () => {
        try {
          await deleteRule(id, rule.id)
          message.success('规则已删除')
          setRules((prev) => prev.filter((r) => r.id !== rule.id))
        } catch {
          message.error('删除失败')
        }
      },
    })
  }

  const columns: ColumnsType<CollectionRule> = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      ellipsis: true,
    },
    {
      title: '模式',
      dataIndex: 'mode',
      key: 'mode',
      width: 110,
      render: (mode: string) => (
        <Tag color={mode === 'WATCH' ? 'blue' : 'purple'}>{mode}</Tag>
      ),
    },
    {
      title: '源路径',
      dataIndex: 'source_path',
      key: 'source_path',
      ellipsis: true,
    },
    {
      title: '文件过滤',
      dataIndex: 'file_pattern',
      key: 'file_pattern',
      width: 130,
    },
    {
      title: 'Cron',
      dataIndex: 'cron_expr',
      key: 'cron_expr',
      width: 140,
      render: (v: string | null) => v ?? '—',
    },
    {
      title: '状态',
      dataIndex: 'is_active',
      key: 'is_active',
      width: 80,
      render: (v: boolean) => <Switch checked={v} size="small" disabled />,
    },
    {
      title: '操作',
      key: 'action',
      width: 100,
      render: (_, rule) => (
        <Button
          size="small"
          danger
          icon={<DeleteOutlined />}
          onClick={() => handleDelete(rule)}
        >
          删除
        </Button>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Space>
          <Button onClick={() => navigate(`/agents/${id}`)}>← 返回详情</Button>
          <Title level={3} style={{ margin: 0 }}>采集规则</Title>
          <Typography.Text type="secondary">采集器 {id}</Typography.Text>
        </Space>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => navigate(`/agents/${id}/rules/create`)}
        >
          新建规则
        </Button>
      </Space>

      <Table
        dataSource={rules}
        columns={columns}
        rowKey="id"
        loading={loading}
        pagination={{ pageSize: 20 }}
        locale={{ emptyText: '暂无采集规则' }}
      />
    </div>
  )
}

export default AgentRulesPage
