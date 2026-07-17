import { useState, useEffect } from 'react'
import {
  App,
  Typography,
  Tabs,
  Descriptions,
  Button,
  Space,
  Modal,
  Table,
  Tag,
  Spin,
  Switch,
  Card,
  Form,
  Input,
} from 'antd'
import {
  CheckCircleOutlined,
  StopOutlined,
  PlusOutlined,
  DeleteOutlined,
  EditOutlined,
  FolderOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useParams, useNavigate } from 'react-router-dom'
import {
  getAgent,
  approveAgent,
  revokeAgent,
  renameAgent,
  listRules,
  deleteRule,
  listDir,
  listAgentUploadLogs,
} from '../../services/agents'
import type { Agent, CollectionRule } from '../../services/agents'
import type { UploadLog } from '../../services/upload-logs'
import AgentStatusBadge from '../../components/AgentStatusBadge'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import DirectoryTree from '../../components/DirectoryTree'
import type { DirEntry } from '../../components/DirectoryTree'

const { Title } = Typography

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

/**
 * Agent detail page — shows 4 tabs: basic info, collection rules, upload logs,
 * and a directory browser backed by the list-dir API.
 */
function AgentDetailPage() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { modal, message } = App.useApp()

  const [agent, setAgent] = useState<Agent | null>(null)
  const [loadingAgent, setLoadingAgent] = useState(true)

  const [renameOpen, setRenameOpen] = useState(false)
  const [renaming, setRenaming] = useState(false)
  const [renameForm] = Form.useForm<{ name: string }>()

  const [rules, setRules] = useState<CollectionRule[]>([])
  const [loadingRules, setLoadingRules] = useState(false)

  const [logs, setLogs] = useState<UploadLog[]>([])
  const [loadingLogs, setLoadingLogs] = useState(false)

  const [dirPath, setDirPath] = useState('/')
  const [dirEntries, setDirEntries] = useState<DirEntry[]>([])
  const [loadingDir, setLoadingDir] = useState(false)
  const [dirModalOpen, setDirModalOpen] = useState(false)

  const [activeTab, setActiveTab] = useState('info')

  useEffect(() => {
    if (!id) return
    setLoadingAgent(true)
    getAgent(id)
      .then(setAgent)
      .catch(() => message.error('获取采集器信息失败'))
      .finally(() => setLoadingAgent(false))
  }, [id])

  useEffect(() => {
    if (activeTab === 'rules' && id) {
      setLoadingRules(true)
      listRules(id)
        .then((data) => setRules(data.items))
        .catch(() => message.error('获取规则列表失败'))
        .finally(() => setLoadingRules(false))
    }
    if (activeTab === 'logs' && id) {
      setLoadingLogs(true)
      listAgentUploadLogs(id, { limit: 50 })
        .then((data) => setLogs(data.items))
        .catch(() => message.error('获取上传日志失败'))
        .finally(() => setLoadingLogs(false))
    }
  }, [activeTab, id])

  const loadDir = async (path: string) => {
    if (!id) return
    setLoadingDir(true)
    setDirPath(path)
    try {
      const result = await listDir(id, path)
      setDirEntries(result.entries)
    } catch {
      message.error('获取目录列表失败')
      setDirEntries([])
    } finally {
      setLoadingDir(false)
    }
  }

  const handleApprove = () => {
    if (!agent) return
    modal.confirm({
      title: `审批采集器：${agent.name}`,
      content: '确认批准该采集器连接系统？',
      onOk: async () => {
        try {
          const updated = await approveAgent(agent.id)
          setAgent(updated)
          message.success('审批成功')
        } catch {
          message.error('审批失败')
        }
      },
    })
  }

  const handleRevoke = () => {
    if (!agent) return
    modal.confirm({
      title: `吊销采集器：${agent.name}`,
      content: '吊销后该采集器将无法连接，确认操作？',
      okType: 'danger',
      onOk: async () => {
        try {
          const updated = await revokeAgent(agent.id)
          setAgent(updated)
          message.success('吊销成功')
        } catch {
          message.error('吊销失败')
        }
      },
    })
  }

  const openRename = () => {
    if (!agent) return
    // Reset first so a prior validation error/touched state doesn't carry over
    // when the modal is reopened, then prefill with the current name.
    renameForm.resetFields()
    renameForm.setFieldsValue({ name: agent.name })
    setRenameOpen(true)
  }

  const handleRename = async () => {
    if (!agent) return
    const values = await renameForm.validateFields().catch(() => null)
    if (!values) return
    setRenaming(true)
    try {
      const updated = await renameAgent(agent.id, values.name.trim())
      setAgent(updated)
      setRenameOpen(false)
      message.success('重命名成功')
    } catch {
      message.error('重命名失败，请稍后重试')
    } finally {
      setRenaming(false)
    }
  }

  const handleDeleteRule = (rule: CollectionRule) => {
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
          message.error('删除规则失败')
        }
      },
    })
  }

  const ruleColumns: ColumnsType<CollectionRule> = [
    { title: '名称', dataIndex: 'name', key: 'name' },
    {
      title: '模式',
      dataIndex: 'mode',
      key: 'mode',
      width: 100,
      render: (mode: string) => (
        <Tag color={mode === 'WATCH' ? 'blue' : 'purple'}>{mode}</Tag>
      ),
    },
    { title: '源路径', dataIndex: 'base_path', key: 'base_path', ellipsis: true },
    { title: '文件过滤', dataIndex: 'path_pattern', key: 'path_pattern', width: 130 },
    {
      title: 'Cron',
      dataIndex: 'cron_expr',
      key: 'cron_expr',
      width: 140,
      render: (v: string | null) => v ?? '-',
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      key: 'enabled',
      width: 80,
      render: (v: boolean) => <Switch checked={v} size="small" disabled />,
    },
    {
      title: '操作',
      key: 'action',
      width: 160,
      render: (_, rule) => (
        <Space>
          <Button
            size="small"
            icon={<EditOutlined />}
            onClick={() => navigate(`/agents/${id}/rules/${rule.id}/edit`)}
          >
            编辑
          </Button>
          <Button
            size="small"
            danger
            icon={<DeleteOutlined />}
            onClick={() => handleDeleteRule(rule)}
          >
            删除
          </Button>
        </Space>
      ),
    },
  ]

  const logColumns: ColumnsType<UploadLog> = [
    { title: '文件名', dataIndex: 'filename', key: 'filename', ellipsis: true },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 100,
      render: (_, row) => formatBytes(row.size),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 90,
      render: (_, row) => <StatusBadge status={row.status} domain="upload" />,
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 150,
      render: (_, row) => <TimeText value={row.uploaded_at} />,
    },
  ]

  if (loadingAgent) {
    return (
      <Spin
        size="large"
        style={{ display: 'block', textAlign: 'center', marginTop: 80 }}
      />
    )
  }

  if (!agent) {
    return <Typography.Text type="danger">采集器不存在或加载失败</Typography.Text>
  }

  const canApprove = agent.status === 'PENDING'
  const canRevoke = ['RUNNING', 'APPROVED', 'OFFLINE'].includes(agent.status)

  const tabs = [
    {
      key: 'info',
      label: '基本信息',
      children: (
        <Card>
          <Descriptions column={2} bordered>
            <Descriptions.Item label="ID">{agent.id}</Descriptions.Item>
            <Descriptions.Item label="名称">{agent.name}</Descriptions.Item>
            <Descriptions.Item label="主机名">{agent.hostname}</Descriptions.Item>
            <Descriptions.Item label="IP 地址">{agent.ip_address}</Descriptions.Item>
            <Descriptions.Item label="操作系统">{agent.os_type}</Descriptions.Item>
            <Descriptions.Item label="版本">{agent.agent_version}</Descriptions.Item>
            <Descriptions.Item label="状态">
              {/* Effective status (badge already reflects is_online) — no
                  separate 实时在线 row, so list and detail never disagree. */}
              <AgentStatusBadge status={agent.status} isOnline={agent.is_online} />
            </Descriptions.Item>
            <Descriptions.Item label="最后心跳">
              <TimeText value={agent.last_seen_at} relative />
            </Descriptions.Item>
            <Descriptions.Item label="注册时间">
              <TimeText value={agent.created_at} />
            </Descriptions.Item>
          </Descriptions>
        </Card>
      ),
    },
    {
      key: 'rules',
      label: '采集规则',
      children: (
        <div>
          <div style={{ marginBottom: 12 }}>
            <Button
              type="primary"
              icon={<PlusOutlined />}
              onClick={() => navigate(`/agents/${id}/rules/create`)}
            >
              新建规则
            </Button>
          </div>
          <Table
            dataSource={rules}
            columns={ruleColumns}
            rowKey="id"
            loading={loadingRules}
            pagination={{ pageSize: 10 }}
            locale={{ emptyText: '暂无采集规则' }}
          />
        </div>
      ),
    },
    {
      key: 'logs',
      label: '上传日志',
      children: (
        <Table
          dataSource={logs}
          columns={logColumns}
          rowKey="id"
          loading={loadingLogs}
          pagination={{ pageSize: 20 }}
          locale={{ emptyText: '暂无上传记录' }}
        />
      ),
    },
    {
      key: 'dir',
      label: '目录浏览',
      disabled: !agent.is_online,
      children: (
        <Card>
          {!agent.is_online ? (
            <Typography.Text type="secondary">采集器当前离线，目录浏览不可用。</Typography.Text>
          ) : (
            <>
              <Space style={{ marginBottom: 12 }}>
                <Button
                  icon={<FolderOutlined />}
                  onClick={() => {
                    setDirModalOpen(true)
                    loadDir('/')
                  }}
                >
                  浏览目录
                </Button>
                <Typography.Text type="secondary">当前路径：{dirPath}</Typography.Text>
              </Space>

              <Modal
                title="目录浏览"
                open={dirModalOpen}
                onCancel={() => setDirModalOpen(false)}
                footer={null}
                width={640}
              >
                <Spin spinning={loadingDir}>
                  <DirectoryTree
                    entries={dirEntries}
                    currentPath={dirPath}
                    onNavigate={loadDir}
                  />
                </Spin>
              </Modal>
            </>
          )}
        </Card>
      ),
    },
  ]

  return (
    <div>
      <Space
        style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}
        wrap
      >
        <Space>
          <Button onClick={() => navigate('/agents')}>← 返回列表</Button>
          <Title level={4} style={{ margin: 0 }}>
            {agent.name}
          </Title>
          <Button
            type="text"
            size="small"
            icon={<EditOutlined />}
            aria-label="重命名"
            title="重命名"
            onClick={openRename}
          />
          <AgentStatusBadge status={agent.status} isOnline={agent.is_online} />
        </Space>
        <Space>
          {canApprove && (
            <Button
              type="primary"
              icon={<CheckCircleOutlined />}
              onClick={handleApprove}
            >
              审批
            </Button>
          )}
          {canRevoke && (
            <Button danger icon={<StopOutlined />} onClick={handleRevoke}>
              吊销
            </Button>
          )}
        </Space>
      </Space>

      <Tabs activeKey={activeTab} onChange={setActiveTab} items={tabs} />

      <Modal
        title="重命名采集器"
        open={renameOpen}
        onOk={handleRename}
        onCancel={() => setRenameOpen(false)}
        confirmLoading={renaming}
        okText="保存"
        cancelText="取消"
        destroyOnHidden
      >
        <Form form={renameForm} layout="vertical" preserve={false}>
          <Form.Item
            name="name"
            label="显示名称"
            rules={[
              { required: true, whitespace: true, message: '请输入名称' },
              { max: 64, message: '名称不超过 64 个字符' },
              {
                pattern: /^[\p{L}\p{N} ._-]+$/u,
                message: '仅允许字母、数字、空格及 . _ -',
              },
            ]}
          >
            <Input placeholder="自定义显示名称" maxLength={64} onPressEnter={handleRename} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

export default AgentDetailPage
