import { useRef, useState } from 'react'
import { App, Button, DatePicker, Input, Radio, Select, Space, Tag, Typography } from 'antd'
import { SearchOutlined, TagsOutlined, PlusOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import useSWR from 'swr'
import { listFiles, batchTagFiles } from '../../services/files'
import type { FileEntry, BatchTagFilter, FileStatusFilter } from '../../services/files'
import { listTagKeys, listTagValues } from '../../services/tags'
import useAuthStore from '../../store/auth'
import BatchDownload from '../../components/BatchDownload'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'
import FormDrawer from '../../components/FormDrawer'
import { useDangerConfirm } from '../../hooks/useDangerConfirm'
import FileDetailDrawer from './FileDetailDrawer'
import type { RangePickerProps } from 'antd/es/date-picker'

const { Title, Text } = Typography
const { RangePicker } = DatePicker

/** Format bytes to human-readable size */
function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`
}

// Values are the raw file_status enum the backend filter expects (lowercase);
// the table renders the uppercased form the API returns.
const STATUS_OPTIONS: { label: string; value: FileStatusFilter | '' }[] = [
  { label: '全部', value: '' },
  { label: '已完成', value: 'completed' },
  { label: '上传中', value: 'uploading' },
  { label: '失败', value: 'failed' },
  { label: '已删除', value: 'deleted' },
]

/**
 * File browser page (WR-4 / metadata 6c screen 7b) — single-line filter bar with
 * tag predicates, selection-driven batch download, file detail in a 480px drawer
 * (3a), and super_admin bulk tagging in a drawer.
 */
function FilesPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  // Server-side filters (status/tag) are cached by key so typing in the
  // client-side filename filter / picking a date range doesn't refetch. The
  // backend's GET /files only supports status/tag/agent/bucket/file_type (it 400s
  // on unknown params), so filename/date are applied client-side over the loaded
  // page. Server-side filename/date is a backlog follow-up.
  const cacheRef = useRef<{ key: string; items: FileEntry[] } | null>(null)
  const user = useAuthStore((s) => s.user)
  const isSuperAdmin = user?.role === 'super_admin'

  const [selectedRowKeys, setSelectedRowKeys] = useState<React.Key[]>([])
  const [filterStatus, setFilterStatus] = useState<FileStatusFilter | ''>('')
  const [filterFilename, setFilterFilename] = useState('')
  const [filterRange, setFilterRange] = useState<[string, string] | null>(null)
  const [filterTags, setFilterTags] = useState<string[]>([])
  const [batchOpen, setBatchOpen] = useState(false)
  const [detailFileId, setDetailFileId] = useState<string | null>(null)

  // Only update state; the table re-requests via its `params` prop. A manual
  // reload would fire an extra request with the stale value from the closure.
  const handleRangeChange: RangePickerProps['onChange'] = (_, dateStrings) => {
    if (dateStrings[0] && dateStrings[1]) {
      setFilterRange([dateStrings[0], dateStrings[1]])
    } else {
      setFilterRange(null)
    }
  }

  const addTagFilter = (kv: string) => {
    const key = kv.slice(0, kv.indexOf(':'))
    setFilterTags((prev) => {
      // Two values for the same key can never both match (AND semantics) and the
      // backend rejects it, so replace any existing predicate for this key.
      const others = prev.filter((t) => t.slice(0, t.indexOf(':')) !== key)
      return [...others, kv]
    })
  }
  const removeTagFilter = (kv: string) => {
    setFilterTags((prev) => prev.filter((t) => t !== kv))
  }

  const columns: ProColumns<FileEntry>[] = [
    {
      title: '文件名',
      dataIndex: 'filename',
      key: 'filename',
      ellipsis: true,
      render: (_, file) => (
        <Button
          type="link"
          style={{ padding: 0, height: 'auto' }}
          onClick={() => setDetailFileId(file.id)}
        >
          {file.filename}
        </Button>
      ),
    },
    {
      title: '标签',
      key: 'tags',
      width: 200,
      render: (_, file) => {
        const tags = file.tags ?? {}
        const keys = Object.keys(tags)
        if (keys.length === 0) return <Text type="secondary">—</Text>
        return (
          <Space size={[4, 4]} wrap>
            {keys.sort().map((k) => (
              <Tag key={k} style={{ margin: 0 }}>
                {k}:{tags[k]}
              </Tag>
            ))}
          </Space>
        )
      },
    },
    { title: 'MIME 类型', dataIndex: 'mime_type', key: 'mime_type', width: 130, ellipsis: true },
    {
      title: '大小',
      dataIndex: 'size',
      key: 'size',
      width: 90,
      render: (_, file) => formatBytes(file.size),
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 90,
      render: (_, file) => <StatusBadge status={file.status} domain="file" />,
    },
    {
      title: '上传时间',
      dataIndex: 'uploaded_at',
      key: 'uploaded_at',
      width: 150,
      render: (_, file) => <TimeText value={file.uploaded_at} />,
    },
  ]

  return (
    <div>
      <Title level={3} style={{ marginBottom: 16 }}>
        文件浏览器
      </Title>

      {/* Single-line filter bar (定则 3) */}
      <Space wrap style={{ marginBottom: 8 }}>
        <Input
          placeholder="文件名搜索"
          prefix={<SearchOutlined />}
          value={filterFilename}
          onChange={(e) => setFilterFilename(e.target.value)}
          style={{ width: 200 }}
          allowClear
        />
        <Select
          value={filterStatus}
          onChange={setFilterStatus}
          options={STATUS_OPTIONS}
          style={{ width: 120 }}
          placeholder="状态筛选"
        />
        <RangePicker showTime={false} onChange={handleRangeChange} placeholder={['开始日期', '结束日期']} />
        <TagFacetPicker onAdd={addTagFilter} />
        {isSuperAdmin && (
          <Button icon={<TagsOutlined />} onClick={() => setBatchOpen(true)}>
            批量打标
          </Button>
        )}
      </Space>

      {/* Active tag filters (AND-combined) */}
      {filterTags.length > 0 && (
        <Space size={[4, 4]} wrap style={{ marginBottom: 12 }}>
          <Text type="secondary">标签筛选：</Text>
          {filterTags.map((kv) => (
            <Tag key={kv} color="blue" closable onClose={() => removeTagFilter(kv)}>
              {kv}
            </Tag>
          ))}
        </Space>
      )}

      {/* Selection toolbar (1d) — appears once rows are checked. */}
      {selectedRowKeys.length > 0 && (
        <Space style={{ marginBottom: 12 }}>
          <Text type="secondary">已选 {selectedRowKeys.length} 项</Text>
          <BatchDownload fileIds={selectedRowKeys.map(String)} />
          <Button size="small" type="text" onClick={() => setSelectedRowKeys([])}>
            清除选择
          </Button>
        </Space>
      )}

      <ProTable<FileEntry>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20 }}
        rowSelection={{ selectedRowKeys, onChange: (keys) => setSelectedRowKeys(keys) }}
        locale={{ emptyText: <EmptyState description="没有匹配的文件" /> }}
        request={async () => {
          try {
            // Only status/tag go to the server (the rest 400s). Cache the result
            // keyed by those so client-side filename/date filtering is free.
            // Sort the tag predicates so the key (and request) are order-stable —
            // {A,B} and {B,A} are the same AND-filter and must hit the same cache.
            const sortedTags = [...filterTags].sort()
            const serverKey = JSON.stringify({ filterStatus, filterTags: sortedTags })
            let items = cacheRef.current?.key === serverKey ? cacheRef.current.items : null
            if (!items) {
              const params: Record<string, unknown> = { limit: 100 }
              if (filterStatus) params.status = filterStatus
              if (sortedTags.length > 0) params.tag = sortedTags
              const data = await listFiles(params as Parameters<typeof listFiles>[0])
              items = data.items
              cacheRef.current = { key: serverKey, items }
            }
            // Client-side filename substring + date-range (on uploaded_at date).
            let rows = items
            if (filterFilename) {
              const q = filterFilename.toLowerCase()
              rows = rows.filter((f) => f.filename.toLowerCase().includes(q))
            }
            if (filterRange) {
              const [from, to] = filterRange
              rows = rows.filter((f) => {
                if (!f.uploaded_at) return false
                // Compare by LOCAL calendar day (matching how TimeText renders the
                // time) so files near midnight aren't filtered by a different UTC day.
                const dt = new Date(f.uploaded_at)
                if (Number.isNaN(dt.getTime())) return false // invalid date → exclude
                const y = dt.getFullYear()
                const m = String(dt.getMonth() + 1).padStart(2, '0')
                const d = String(dt.getDate()).padStart(2, '0')
                const local = `${y}-${m}-${d}`
                return local >= from && local <= to
              })
            }
            return { data: rows, success: true, total: rows.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
        params={{ filterStatus, filterFilename, filterRange, filterTags }}
      />

      <FileDetailDrawer fileId={detailFileId} onClose={() => setDetailFileId(null)} />

      {isSuperAdmin && (
        <BatchTagDrawer
          open={batchOpen}
          status={filterStatus}
          tags={filterTags}
          onClose={() => setBatchOpen(false)}
          onDone={() => {
            setBatchOpen(false)
            cacheRef.current = null // tags changed → drop cached page
            actionRef.current?.reload()
          }}
        />
      )}
    </div>
  )
}

/** Key+value picker that emits a `key:value` predicate to add as a tag filter.
 * Values are the key's registered vocabulary (unapproved values are not offered,
 * per governance). */
function TagFacetPicker({ onAdd }: { onAdd: (kv: string) => void }) {
  const { data: keys = [] } = useSWR('tag-keys', listTagKeys)
  const [key, setKey] = useState<string | undefined>()
  const [value, setValue] = useState<string | undefined>()
  const { data: values = [], isLoading } = useSWR(key ? ['tag-values', key] : null, () =>
    listTagValues(key as string),
  )

  const add = () => {
    if (!key || !value) return
    onAdd(`${key}:${value}`)
    setValue(undefined)
  }

  return (
    <Space.Compact>
      <Select
        placeholder="标签键"
        style={{ width: 120 }}
        value={key}
        onChange={(k) => {
          setKey(k)
          setValue(undefined)
        }}
        options={keys.map((k) => ({ label: k.key, value: k.key }))}
        showSearch
        optionFilterProp="label"
      />
      <Select
        placeholder="取值"
        style={{ width: 130 }}
        value={value}
        onChange={setValue}
        loading={isLoading}
        disabled={!key}
        options={values.map((v) => ({ label: v.value, value: v.value }))}
        showSearch
        optionFilterProp="label"
      />
      <Button icon={<PlusOutlined />} onClick={add} disabled={!key || !value}>
        筛选
      </Button>
    </Space.Compact>
  )
}

/**
 * Bulk-tag drawer. Applies a single set/clear operation to every file matching the
 * current status + tag predicate (the subset of filters the batch-tag API
 * supports — filename/date are intentionally not part of the selection). It shows
 * the matched-file count so the operator knows the blast radius before applying.
 */
function BatchTagDrawer({
  open,
  status,
  tags,
  onClose,
  onDone,
}: {
  open: boolean
  status: FileStatusFilter | ''
  tags: string[]
  onClose: () => void
  onDone: () => void
}) {
  const { message } = App.useApp()
  const dangerConfirm = useDangerConfirm()
  const { data: keys = [] } = useSWR('tag-keys', listTagKeys)
  const [key, setKey] = useState<string | undefined>()
  const [mode, setMode] = useState<'set' | 'clear'>('set')
  const [value, setValue] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Order-stable tag predicates: sorting keeps the SWR key and request identical
  // regardless of insertion order, and spreading the array (vs join) avoids key
  // collisions when a tag value contains the separator.
  const sortedTags = [...tags].sort()

  // Preview the blast radius: count files matching the same predicate.
  const { data: preview } = useSWR(open ? ['batch-count', status, ...sortedTags] : null, () =>
    listFiles({ status: status || undefined, tag: sortedTags.length ? sortedTags : undefined, limit: 1 }),
  )

  const apply = async () => {
    if (!key) return
    setSubmitting(true)
    try {
      const filter: BatchTagFilter = {}
      if (status) filter.status = status
      if (sortedTags.length) filter.tags = sortedTags
      await batchTagFiles(filter, { [key]: mode === 'set' ? value.trim() : null })
      message.success('已提交批量打标任务，将在后台执行')
      onDone()
    } catch (err) {
      const status2 = (err as { response?: { status?: number } }).response?.status
      message.error(status2 === 400 ? '标签键未登记或取值非法' : '批量打标失败')
    } finally {
      setSubmitting(false)
    }
  }

  const submit = () => {
    if (submitting) return
    if (!key) {
      message.error('请选择标签键')
      return
    }
    if (mode === 'set' && !value.trim()) {
      message.error('请填写要设置的取值')
      return
    }
    // No filter = 全量变更: require an explicit danger confirm before applying,
    // since it retags every file via an async worker and can't be undone per-file.
    if (!status && tags.length === 0) {
      dangerConfirm({
        title: '对全部文件应用标签？',
        content: '未添加任何筛选，本次打标将作用于全部文件，由后台任务执行且无法逐一撤销。',
        okText: '仍然应用',
        onOk: apply,
      })
      return
    }
    void apply()
  }

  return (
    <FormDrawer
      open={open}
      title="批量打标"
      onClose={onClose}
      onSubmit={submit}
      loading={submitting}
      submitText="应用"
    >
      <Text type="secondary">
        将对<Text strong>匹配当前状态与标签筛选</Text>的全部文件应用标签
        {typeof preview?.total === 'number' && <>（约 {preview.total} 个）</>}。
        文件名/日期筛选不参与圈选。
      </Text>
      <div style={{ marginTop: 8, marginBottom: 12 }}>
        <Space size={[4, 4]} wrap>
          {status && <Tag>状态：{status}</Tag>}
          {tags.map((t) => (
            <Tag key={t} color="blue">
              {t}
            </Tag>
          ))}
          {!status && tags.length === 0 && <Text type="warning">未加筛选：将作用于全部文件</Text>}
        </Space>
      </div>
      <Space direction="vertical" style={{ width: '100%' }} size={12}>
        <Select
          placeholder="标签键"
          style={{ width: '100%' }}
          value={key}
          onChange={setKey}
          options={keys.map((k) => ({ label: `${k.key}（${k.label}）`, value: k.key }))}
          showSearch
          optionFilterProp="label"
        />
        <Radio.Group value={mode} onChange={(e) => setMode(e.target.value)}>
          <Radio value="set">设置取值</Radio>
          <Radio value="clear">清除该标签</Radio>
        </Radio.Group>
        {mode === 'set' && (
          <Input
            placeholder="取值（未登记的受控取值将进入待确认队列）"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onPressEnter={submit}
          />
        )}
      </Space>
    </FormDrawer>
  )
}

export default FilesPage
