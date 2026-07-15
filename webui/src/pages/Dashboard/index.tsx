import { Row, Col, Card, Statistic, Table, Typography, theme } from 'antd'
import {
  RobotOutlined,
  UploadOutlined,
  FileOutlined,
  CloudServerOutlined,
} from '@ant-design/icons'
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from 'recharts'
import useSWR from 'swr'
import type { ColumnsType } from 'antd/es/table'
import { listAgents } from '../../services/agents'
import { listUploadLogs } from '../../services/upload-logs'
import type { UploadLog } from '../../services/upload-logs'
import { getDashboardStats } from '../../services/stats'
import AgentStatusBadge from '../../components/AgentStatusBadge'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'

const { Title } = Typography

/** Format bytes to human-readable storage size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

const uploadLogColumns: ColumnsType<UploadLog> = [
  {
    title: '文件名',
    dataIndex: 'filename',
    key: 'filename',
    ellipsis: true,
  },
  {
    title: '大小',
    dataIndex: 'size',
    key: 'size',
    width: 100,
    render: (size: number) => formatBytes(size),
  },
  {
    title: '状态',
    dataIndex: 'status',
    key: 'status',
    width: 90,
    render: (status: string) => <StatusBadge status={status} domain="upload" />,
  },
  {
    title: '上传时间',
    dataIndex: 'uploaded_at',
    key: 'uploaded_at',
    width: 150,
    render: (t: string) => <TimeText value={t} />,
  },
]

/**
 * Dashboard page — shows system overview with stat cards, 7-day upload trend,
 * agent status list and recent 20 upload logs. SWR auto-refreshes every 30s.
 * Loading uses card skeletons (交互定则 6, 不用转圈); accent colors come from
 * theme tokens (§1.1, 不硬编码).
 */
function DashboardPage() {
  const SWR_OPTS = { refreshInterval: 30_000 }
  const { token } = theme.useToken()

  // Real aggregates from the Control Plane (replaces the earlier client-side
  // approximations derived from a 20-row upload-logs sample).
  const { data: stats, isLoading: loadingStats } = useSWR(
    'dashboard-stats',
    getDashboardStats,
    SWR_OPTS
  )

  // Agents list feeds the online-status table; recent logs feed the log table.
  const { data: agentsData, isLoading: loadingAgents } = useSWR(
    'dashboard-agents',
    () => listAgents({ limit: 100 }),
    SWR_OPTS
  )

  const { data: logsData, isLoading: loadingLogs } = useSWR(
    'dashboard-logs',
    () => listUploadLogs({ limit: 20 }),
    SWR_OPTS
  )

  const agents = agentsData?.items ?? []
  const onlineCount = stats?.online_agents ?? 0
  const totalCount = stats?.total_agents ?? 0
  const totalFiles = stats?.total_files ?? 0
  const totalStorage = stats?.storage_bytes ?? 0
  const todayUploads = stats?.today_uploads ?? 0

  // 7-day upload trend from the API; format ISO dates as short M/D labels.
  const trendData = (stats?.upload_trend ?? []).map((d) => {
    const [, m, day] = d.date.split('-')
    return { date: `${Number(m)}/${Number(day)}`, count: d.count }
  })

  return (
    <div>
      <Title level={3} style={{ marginBottom: 24 }}>仪表盘</Title>

      {/* Stat cards */}
      <Row gutter={[16, 16]} style={{ marginBottom: 24 }}>
        <Col xs={24} sm={12} lg={6}>
          <Card loading={loadingStats}>
            <Statistic
              title="在线采集器"
              value={onlineCount}
              suffix={`/ ${totalCount}`}
              prefix={<RobotOutlined />}
              valueStyle={{ color: token.colorSuccess }}
            />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card loading={loadingStats}>
            <Statistic
              title="今日上传"
              value={todayUploads}
              suffix="个"
              prefix={<UploadOutlined />}
              valueStyle={{ color: token.colorPrimary }}
            />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card loading={loadingStats}>
            <Statistic
              title="总文件数"
              value={totalFiles}
              prefix={<FileOutlined />}
            />
          </Card>
        </Col>
        <Col xs={24} sm={12} lg={6}>
          <Card loading={loadingStats}>
            <Statistic
              title="存储用量"
              value={formatBytes(totalStorage)}
              prefix={<CloudServerOutlined />}
            />
          </Card>
        </Col>
      </Row>

      {/* Chart + Agent status list */}
      <Row gutter={[16, 16]} style={{ marginBottom: 24 }}>
        <Col xs={24} lg={14}>
          <Card title="近7日上传量趋势" loading={loadingStats}>
            {trendData.every((d) => d.count === 0) ? (
              <EmptyState description="暂无上传数据" />
            ) : (
              <ResponsiveContainer width="100%" height={240}>
                <LineChart data={trendData}>
                  <CartesianGrid strokeDasharray="3 3" stroke={token.colorSplit} />
                  <XAxis dataKey="date" />
                  <YAxis allowDecimals={false} />
                  <Tooltip />
                  <Line
                    type="monotone"
                    dataKey="count"
                    name="上传数"
                    stroke={token.colorPrimary}
                    strokeWidth={2}
                    dot={{ r: 3 }}
                  />
                </LineChart>
              </ResponsiveContainer>
            )}
          </Card>
        </Col>
        <Col xs={24} lg={10}>
          <Card title="采集器在线状态" loading={loadingAgents}>
            {agents.length === 0 ? (
              <EmptyState description="暂无采集器" />
            ) : (
              <Table
                dataSource={agents.slice(0, 8)}
                rowKey="id"
                size="small"
                pagination={false}
                columns={[
                  { title: '名称', dataIndex: 'name', key: 'name', ellipsis: true },
                  {
                    title: '状态',
                    dataIndex: 'status',
                    key: 'status',
                    width: 90,
                    render: (s) => <AgentStatusBadge status={s} />,
                  },
                ]}
              />
            )}
          </Card>
        </Col>
      </Row>

      {/* Recent upload logs */}
      <Card title="最近上传日志（最新20条）" loading={loadingLogs}>
        <Table<UploadLog>
          dataSource={logsData?.items ?? []}
          columns={uploadLogColumns}
          rowKey="id"
          pagination={false}
          size="small"
          locale={{ emptyText: <EmptyState description="暂无上传记录" /> }}
        />
      </Card>
    </div>
  )
}

export default DashboardPage
