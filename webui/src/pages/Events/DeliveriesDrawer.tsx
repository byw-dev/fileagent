import useSWR from 'swr'
import { Drawer, Table, Typography, Descriptions } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { listRuleDeliveries } from '../../services/events'
import type { EventDelivery, EventRule } from '../../services/events'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'

const { Text, Paragraph } = Typography

/** A delivery is "failed-ish" (expandable for diagnostics) when failed or dead. */
function isFailed(status: string): boolean {
  const s = status.toUpperCase()
  return s === 'FAILED' || s === 'DEAD'
}

interface DeliveriesDrawerProps {
  /** Rule whose deliveries to show; null closes the drawer. */
  rule: EventRule | null
  onClose: () => void
}

/**
 * DeliveriesDrawer — delivery history for one event rule in a right drawer
 * (4c, 交互定则 1). Status uses the site-wide badge (含终态 dead); failed/dead rows
 * expand to show the response code / body / retry timing.
 */
function DeliveriesDrawer({ rule, onClose }: DeliveriesDrawerProps) {
  // Fetch only while a rule is selected; the key changes with the rule so
  // switching rules refetches, and closing (null) drops the request.
  const { data, isLoading } = useSWR(
    rule ? ['rule-deliveries', rule.id] : null,
    () => listRuleDeliveries(rule!.id, { limit: 50 }),
  )
  const deliveries = data?.items ?? []
  const loading = isLoading

  const columns: ColumnsType<EventDelivery> = [
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 96,
      render: (status: string) => <StatusBadge status={status} domain="delivery" />,
    },
    {
      title: '尝试次数',
      dataIndex: 'attempt_count',
      key: 'attempt_count',
      width: 84,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      render: (v: string) => <TimeText value={v} />,
    },
  ]

  return (
    <Drawer
      open={rule !== null}
      title={rule ? `投递记录：${rule.name}` : '投递记录'}
      onClose={onClose}
      width={480}
      destroyOnHidden
    >
      <Table<EventDelivery>
        dataSource={deliveries}
        columns={columns}
        rowKey="id"
        size="small"
        loading={loading}
        pagination={false}
        locale={{ emptyText: <EmptyState description="暂无投递记录" /> }}
        expandable={{
          // Only failed/dead rows carry diagnostics worth expanding.
          rowExpandable: (d) => isFailed(d.status),
          expandedRowRender: (d) => (
            <Descriptions size="small" column={1} style={{ margin: 0 }}>
              <Descriptions.Item label="HTTP 状态">
                {d.response_code ?? <Text type="secondary">—</Text>}
              </Descriptions.Item>
              <Descriptions.Item label="下次重试">
                {d.next_retry_at ? (
                  <TimeText value={d.next_retry_at} />
                ) : (
                  <Text type="secondary">无（已终止）</Text>
                )}
              </Descriptions.Item>
              <Descriptions.Item label="响应体">
                {d.response_body ? (
                  <Paragraph
                    style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}
                    code
                  >
                    {d.response_body}
                  </Paragraph>
                ) : (
                  <Text type="secondary">无</Text>
                )}
              </Descriptions.Item>
            </Descriptions>
          ),
        }}
      />
    </Drawer>
  )
}

export default DeliveriesDrawer
