import { useRef } from 'react'
import { App, Alert, Button, Space, Typography } from 'antd'
import { CheckCircleOutlined, StopOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useNavigate } from 'react-router-dom'
import useSWR from 'swr'
import { listAgents, approveAgent, revokeAgent } from '../../services/agents'
import type { Agent } from '../../services/agents'
import AgentStatusBadge from '../../components/AgentStatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'
import { useDangerConfirm } from '../../hooks/useDangerConfirm'

const { Title } = Typography

/**
 * Agents list page (WR-3). A 待审批置顶横幅 (1c) surfaces pending agents instead
 * of a status tab bar; status uses the site-wide badge reconciled with the
 * real-time online flag (定则 5 relative heartbeat); approve/revoke run inline
 * (revoke via the danger confirm, 定则 2).
 */
function AgentsPage() {
  const navigate = useNavigate()
  const actionRef = useRef<ActionType | undefined>(undefined)
  const { modal, message } = App.useApp()
  const dangerConfirm = useDangerConfirm()

  // Lightweight pending count for the top banner (limit 1 → read total only).
  const { data: pending, mutate: refetchPending } = useSWR('agents-pending-count', () =>
    listAgents({ status: 'PENDING', limit: 1 }),
  )
  const pendingCount = pending?.total ?? 0

  const reload = () => {
    actionRef.current?.reload()
    void refetchPending()
  }

  const handleApprove = (agent: Agent) => {
    modal.confirm({
      title: `审批采集器：${agent.name}`,
      content: '确认批准该采集器连接系统？',
      okText: '审批',
      cancelText: '取消',
      onOk: async () => {
        try {
          await approveAgent(agent.id)
          message.success('审批成功')
          reload()
        } catch {
          message.error('审批失败')
        }
      },
    })
  }

  const handleRevoke = (agent: Agent) => {
    dangerConfirm({
      title: `吊销采集器：${agent.name}`,
      content: '吊销后该采集器将无法连接，需重新审批。此操作请谨慎。',
      okText: '吊销',
      onOk: async () => {
        try {
          await revokeAgent(agent.id)
          message.success('吊销成功')
          reload()
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
        <Button
          type="link"
          style={{ padding: 0, height: 'auto' }}
          onClick={() => navigate(`/agents/${agent.id}`)}
        >
          {agent.name}
        </Button>
      ),
    },
    { title: '主机名', dataIndex: 'hostname', key: 'hostname', ellipsis: true },
    { title: 'IP 地址', dataIndex: 'ip_address', key: 'ip_address', width: 140 },
    { title: '操作系统', dataIndex: 'os_type', key: 'os_type', width: 100 },
    { title: '版本', dataIndex: 'agent_version', key: 'agent_version', width: 90 },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 100,
      render: (_, agent) => <AgentStatusBadge status={agent.status} isOnline={agent.is_online} />,
    },
    {
      title: '最后心跳',
      dataIndex: 'last_seen_at',
      key: 'last_seen_at',
      width: 130,
      render: (_, agent) => <TimeText value={agent.last_seen_at} relative />,
    },
    {
      title: '注册时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 150,
      render: (_, agent) => <TimeText value={agent.created_at} />,
    },
    {
      title: '操作',
      key: 'action',
      width: 150,
      align: 'right',
      render: (_, agent) => (
        <Space size="middle">
          {agent.status === 'PENDING' && (
            <Button
              type="link"
              icon={<CheckCircleOutlined />}
              style={{ padding: 0, height: 'auto' }}
              onClick={() => handleApprove(agent)}
            >
              审批
            </Button>
          )}
          {(agent.status === 'RUNNING' ||
            agent.status === 'APPROVED' ||
            agent.status === 'OFFLINE') && (
            <Button
              type="link"
              danger
              icon={<StopOutlined />}
              style={{ padding: 0, height: 'auto' }}
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

      {/* 待审批置顶横幅 (1c) — only when there is something to review. */}
      {pendingCount > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message={`有 ${pendingCount} 个采集器待审批`}
          action={
            <Button size="small" type="primary" onClick={() => navigate('/agents/pending')}>
              去审批
            </Button>
          }
        />
      )}

      <ProTable<Agent>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        locale={{ emptyText: <EmptyState description="还没有采集器" /> }}
        request={async () => {
          try {
            const data = await listAgents({ limit: 100 })
            return { data: data.items, success: true, total: data.total }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />
    </div>
  )
}

export default AgentsPage
