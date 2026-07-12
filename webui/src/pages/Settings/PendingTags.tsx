import { useRef, useState } from 'react'
import { App, Button, Modal, Select, Space, Tag, Typography } from 'antd'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import useSWR from 'swr'
import useAuthStore from '../../store/auth'
import {
  listPendingTagValues,
  approvePendingTagValue,
  mergePendingTagValue,
  rejectPendingTagValue,
} from '../../services/pending-tags'
import type { PendingTagValue } from '../../services/pending-tags'
import { listTagValues } from '../../services/tags'

const { Title, Text } = Typography

const sourceLabels: Record<string, string> = {
  path_var: '路径变量',
  path_backfill: '回填',
  manual: '手动',
}

/**
 * Pending tag-value review queue (metadata 6c, screen 7d). Lists values seen by
 * path extraction / manual tagging that are not yet in a controlled key's
 * vocabulary. Listing is open to any user; approve/merge/reject are super_admin.
 */
function PendingTagsPage() {
  const user = useAuthStore((s) => s.user)
  const isSuperAdmin = user?.role === 'super_admin'
  const { modal, message } = App.useApp()
  const actionRef = useRef<ActionType | undefined>(undefined)
  const [mergeRow, setMergeRow] = useState<PendingTagValue | null>(null)

  const reload = () => actionRef.current?.reload()

  const approve = async (row: PendingTagValue) => {
    try {
      await approvePendingTagValue(row.id)
      message.success(`已核准「${row.extracted_value}」入词表`)
      reload()
    } catch {
      message.error('核准失败')
    }
  }

  const reject = (row: PendingTagValue) => {
    modal.confirm({
      title: `拒绝取值：${row.extracted_value}`,
      content: '拒绝后该取值移出队列且不入词表，已打标文件保留原值。确认拒绝？',
      okType: 'danger',
      onOk: async () => {
        try {
          await rejectPendingTagValue(row.id)
          message.success('已拒绝')
          reload()
        } catch {
          message.error('拒绝失败')
        }
      },
    })
  }

  const columns: ProColumns<PendingTagValue>[] = [
    { title: '键', dataIndex: 'key', key: 'key', render: (_, r) => <Text code>{r.key}</Text> },
    { title: '取值', dataIndex: 'extracted_value', key: 'extracted_value' },
    {
      title: '来源',
      dataIndex: 'source',
      key: 'source',
      width: 100,
      render: (_, r) => <Tag>{sourceLabels[r.source] ?? r.source}</Tag>,
    },
    { title: '命中', dataIndex: 'hit_count', key: 'hit_count', width: 80 },
    {
      title: '疑似',
      dataIndex: 'suggested_value',
      key: 'suggested_value',
      width: 120,
      render: (_, r) => (r.suggested_value ? <Tag color="gold">{r.suggested_value}</Tag> : '—'),
    },
    {
      title: '首次出现',
      dataIndex: 'first_seen_at',
      key: 'first_seen_at',
      width: 170,
      render: (_, r) => new Date(r.first_seen_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 200,
      render: (_, r) =>
        isSuperAdmin ? (
          <Space size="small">
            <Button size="small" type="primary" onClick={() => approve(r)}>
              核准
            </Button>
            <Button size="small" onClick={() => setMergeRow(r)}>
              并入
            </Button>
            <Button size="small" danger onClick={() => reject(r)}>
              拒绝
            </Button>
          </Space>
        ) : (
          <Text type="secondary">仅超级管理员可处理</Text>
        ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>
          待确认取值
        </Title>
        <Button onClick={reload}>刷新</Button>
      </Space>

      <ProTable<PendingTagValue>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={false}
        options={false}
        request={async () => {
          try {
            const data = await listPendingTagValues()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      {mergeRow && (
        <MergeModal
          key={mergeRow.id}
          row={mergeRow}
          onClose={() => setMergeRow(null)}
          onDone={() => {
            setMergeRow(null)
            reload()
          }}
        />
      )}
    </div>
  )
}

/**
 * Modal to merge a pending value into an existing canonical value of its key.
 * The target must be a registered value (the API enforces this), so it is chosen
 * from a Select of the key's vocabulary, defaulting to the suggested value.
 */
function MergeModal({
  row,
  onClose,
  onDone,
}: {
  row: PendingTagValue
  onClose: () => void
  onDone: () => void
}) {
  const { message } = App.useApp()
  const { data: values = [], isLoading } = useSWR(['tag-values', row.key], () =>
    listTagValues(row.key),
  )
  const [into, setInto] = useState<string | undefined>(row.suggested_value)
  const [submitting, setSubmitting] = useState(false)

  const submit = async () => {
    if (!into || submitting) return
    setSubmitting(true)
    try {
      await mergePendingTagValue(row.id, into)
      message.success('已提交并入任务，回溯改写将在后台执行')
      onDone()
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status
      message.error(status === 400 ? '目标取值必须已在词表中且不同于原值' : '并入失败')
    } finally {
      setSubmitting(false)
    }
  }

  // A value can only be merged into a *different* registered value of the same key.
  const options = values
    .filter((v) => v.value !== row.extracted_value)
    .map((v) => ({ label: v.value, value: v.value }))

  return (
    <Modal
      open
      title={`并入取值：${row.extracted_value}`}
      okText="并入"
      okButtonProps={{ disabled: !into, loading: submitting }}
      onOk={submit}
      onCancel={onClose}
    >
      <Text type="secondary">
        将键 <Text code>{row.key}</Text> 的「{row.extracted_value}」并入下列已登记取值；已打标文件将回溯改写。
      </Text>
      <Select
        style={{ width: '100%', marginTop: 12 }}
        placeholder="选择目标取值"
        loading={isLoading}
        value={into}
        onChange={setInto}
        options={options}
        showSearch
        optionFilterProp="label"
      />
    </Modal>
  )
}

export default PendingTagsPage
