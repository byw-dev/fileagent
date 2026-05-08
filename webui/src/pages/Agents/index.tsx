import { useState } from 'react'
import { Button, Space, Modal, message, Typography, Tabs } from 'antd'
import {
  CheckCircleOutlined,
  StopOutlined,
  PlusOutlined,
} from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useNavigate } from 'react-router-dom'
import { useRef } from 'react'
import {
  listAgents,
  approveAgent,
  revokeAgent,
} from '../../services/agents'
import type { Agent, AgentStatus } from '../../services/agents'
import AgentStatusBadge from '../../components/AgentStatusBadge'

const { Title } = Typography

const STATUS_TABS: { key: string; label: string; status?: AgentStatus }[] = [
  { key: 'all', label: '全部' },
  { key: 'running', label: '运行中', status: 'RUNNING' },
  { key: 'pending', label: '待审批', status: 'PENDING' },
  { key: 'offline', label: '离线', status: 'OFFLINE' },
  { key: 'revoked', label: '已吊销', status: 'REVOKED' },
]

/**
 * Agents list page — ProTable with status tab filter, approve and revoke actions.
 */
function AgentsPage() {
  const navigate = useNavigate()
  const actionRef = useRef<ActionType | undefined>(undefined)
  const [activeTab, setActiveTab] = useState('all')

  const currentStatus = STATUS_TABS.find((t) => t.key === activeTab)?.status

  const handleApprove = (agent: Agent) => {
    Modal.confirm({
      title: `审批采集器：${agent.name}`,
      content: '确认批准该采集器连接系统？',
      onOk: async () => {
        try {
          await approveAgent(agent.id)
          message.success('审批成功')
          actionRef.current?.reload()
        } catch {
          message.error('审批失败')
        }
      },
    })
  }

  const handleRevoke = (agent: Agent) => {
    Modal.confirm({
      title: `吊销采集器：${agent.name}`,
      content: '吊销后该采集器将无法连接，确认操作？',
      okType: 'danger',
      onOk: async () => {
        try {
          await revokeAgent(agent.id)
          message.success('吊销成功')
          actionRef.current?.reload()
        } catch {
          message.error('吊销失败')
        }
      },
    })
  }

  const columns: ProColumns<Agent>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (_, agent) => (
        <a onClick={() => navigate(`/agents/${agent.id}`)}>{agent.name}</a>
      ),
    },
    {
      title: '主机名',
      dataIndex: 'hostname',
      key: 'hostname',
    },
    {
      title: 'IP 地址',
      dataIndex: 'ip_address',
      key: 'ip_address',
      width: 140,
    },
    {
      title: '操作系统',
      dataIndex: 'os_type',
      key: 'os_type',
      width: 100,
    },
    {
      title: '版本',
      dataIndex: 'agent_version',
      key: 'agent_version',
      width: 90,
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (_, agent) => <AgentStatusBadge status={agent.status} />,
    },
    {
      title: '最后心跳',
      dataIndex: 'last_seen_at',
      key: 'last_seen_at',
      width: 170,
      render: (_, agent) =>
        agent.last_seen_at
          ? new Date(agent.last_seen_at).toLocaleString('zh-CN')
          : '-',
    },
    {
      title: '注册时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (_, agent) => new Date(agent.created_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 180,
      render: (_, agent) => (
        <Space>
          <Button
            size="small"
            onClick={() => navigate(`/agents/${agent.id}`)}
          >
            详情
          </Button>
          {agent.status === 'PENDING' && (
            <Button
              size="small"
              type="primary"
              icon={<CheckCircleOutlined />}
              onClick={() => handleApprove(agent)}
            >
              审批
            </Button>
          )}
          {(agent.status === 'RUNNING' || agent.status === 'APPROVED' || agent.status === 'OFFLINE') && (
            <Button
              size="small"
              danger
              icon={<StopOutlined />}
              onClick={() => handleRevoke(agent)}
            >
              吊销
            </Button>
          )}
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>采集器管理</Title>

      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={STATUS_TABS.map((t) => ({ key: t.key, label: t.label }))}
        style={{ marginBottom: 8 }}
      />

      <ProTable<Agent>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        toolBarRender={() => [
          <Button
            key="pending"
            icon={<PlusOutlined />}
            onClick={() => navigate('/agents/pending')}
          >
            待审批列表
          </Button>,
        ]}
        request={async () => {
          try {
            const data = await listAgents({
              status: currentStatus,
              limit: 100,
            })
            return { data: data.items, success: true, total: data.total }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
        params={{ activeTab }}
      />
    </div>
  )
}

export default AgentsPage
