import { App, Button, Space, Typography } from 'antd'
import { CheckCircleOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { listAgents, approveAgent } from '../../services/agents'
import type { Agent } from '../../services/agents'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'

const { Title } = Typography

/**
 * Pending agents page — lists agents awaiting approval with one-click approve action.
 */
function AgentsPendingPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  const navigate = useNavigate()
  const { modal, message } = App.useApp()

  const handleApprove = (agent: Agent) => {
    modal.confirm({
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
      title: '注册时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 150,
      render: (_, agent) => <TimeText value={agent.created_at} />,
    },
    {
      title: '操作',
      key: 'action',
      width: 120,
      render: (_, agent) => (
        <Space>
          <Button
            type="primary"
            size="small"
            icon={<CheckCircleOutlined />}
            onClick={() => handleApprove(agent)}
          >
            审批
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>待审批采集器</Title>

      <ProTable<Agent>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        locale={{ emptyText: <EmptyState description="没有待审批的采集器" /> }}
        request={async () => {
          try {
            const data = await listAgents({ status: 'PENDING', limit: 100 })
            return { data: data.items, success: true, total: data.total }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />
    </div>
  )
}

export default AgentsPendingPage
