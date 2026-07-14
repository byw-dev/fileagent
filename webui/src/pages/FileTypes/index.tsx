import { useRef, useState } from 'react'
import { App, Button, Form, Input, Space, Typography } from 'antd'
import { PlusOutlined, SearchOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import {
  listFileTypes,
  createFileType,
  updateFileType,
  deleteFileType,
} from '../../services/file-types'
import type { FileType } from '../../services/file-types'
import FormDrawer from '../../components/FormDrawer'
import { DangerConfirmModal } from '../../components/DangerConfirm'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'

const { Title, Text } = Typography
const { TextArea } = Input

interface FileTypeFormValues {
  name: string
  description?: string
}

/**
 * File types list page (WR-2 sample page). Establishes the site-wide patterns:
 * create/edit in a 480px FormDrawer (交互定则 1), delete via a name-to-confirm
 * danger modal (4f), MM-DD HH:mm times (定则 5), single-line filter (定则 3),
 * right-aligned action column (定则 4), and a one-line empty state (定则 6).
 *
 * `file_types` is the demoted glob fallback classifier (D-025); glob-rule editing
 * needs a backend API that does not exist yet and is tracked separately (backlog).
 */
function FileTypesPage() {
  const actionRef = useRef<ActionType | undefined>(undefined)
  const { message } = App.useApp()

  const [filterName, setFilterName] = useState('')
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [editing, setEditing] = useState<FileType | null>(null)
  const [form] = Form.useForm<FileTypeFormValues>()
  const [submitting, setSubmitting] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<FileType | null>(null)
  const [deleting, setDeleting] = useState(false)

  // FormDrawer destroys its body on close, so the form remounts on each open and
  // picks up `initialValues` fresh — no imperative setFieldsValue (which would run
  // before the fields mount and be dropped).
  const openCreate = () => {
    setEditing(null)
    setDrawerOpen(true)
  }
  const openEdit = (ft: FileType) => {
    setEditing(ft)
    setDrawerOpen(true)
  }

  const submit = async () => {
    let values: FileTypeFormValues
    try {
      values = await form.validateFields()
    } catch {
      // Validation errors are surfaced inline by the form fields; abort silently
      // so the rejected validateFields promise never escapes as unhandled.
      return
    }
    setSubmitting(true)
    try {
      if (editing) {
        await updateFileType(editing.id, values)
        message.success('已更新文件类型')
      } else {
        await createFileType(values)
        message.success('已创建文件类型')
      }
      setDrawerOpen(false)
      actionRef.current?.reload()
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status
      message.error(status === 409 ? '同名文件类型已存在' : '保存失败')
    } finally {
      setSubmitting(false)
    }
  }

  const confirmDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await deleteFileType(deleteTarget.id)
      message.success('已删除')
      setDeleteTarget(null)
      actionRef.current?.reload()
    } catch {
      message.error('删除失败')
    } finally {
      setDeleting(false)
    }
  }

  const columns: ProColumns<FileType>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (_, ft) => <a onClick={() => openEdit(ft)}>{ft.name}</a>,
    },
    {
      title: '描述',
      dataIndex: 'description',
      key: 'description',
      ellipsis: true,
      render: (_, ft) => ft.description || <Text type="secondary">—</Text>,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 160,
      render: (_, ft) => <TimeText value={ft.created_at} />,
    },
    {
      title: '操作',
      key: 'action',
      width: 130,
      align: 'right',
      render: (_, ft) => (
        <Space size="middle">
          <a onClick={() => openEdit(ft)}>编辑</a>
          <Typography.Link type="danger" onClick={() => setDeleteTarget(ft)}>
            删除
          </Typography.Link>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>
          文件类型
        </Title>
        <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
          新建类型
        </Button>
      </Space>

      {/* Single-line filter (交互定则 3). */}
      <Space style={{ marginBottom: 12 }}>
        <Input
          placeholder="按名称搜索"
          prefix={<SearchOutlined />}
          value={filterName}
          onChange={(e) => setFilterName(e.target.value)}
          style={{ width: 240 }}
          allowClear
        />
      </Space>

      <ProTable<FileType>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        locale={{
          emptyText: (
            <EmptyState
              description={filterName ? '没有匹配的文件类型' : '还没有文件类型'}
              action={
                filterName ? undefined : (
                  <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
                    新建类型
                  </Button>
                )
              }
            />
          ),
        }}
        params={{ filterName }}
        request={async () => {
          try {
            const all = await listFileTypes()
            const data = filterName
              ? all.filter((ft) => ft.name.toLowerCase().includes(filterName.toLowerCase()))
              : all
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      <FormDrawer
        open={drawerOpen}
        title={editing ? `编辑文件类型：${editing.name}` : '新建文件类型'}
        onClose={() => setDrawerOpen(false)}
        onSubmit={submit}
        loading={submitting}
      >
        <Form
          form={form}
          layout="vertical"
          preserve={false}
          initialValues={{ name: editing?.name ?? '', description: editing?.description ?? '' }}
        >
          <Form.Item
            name="name"
            label="名称"
            rules={[{ required: true, whitespace: true, message: '请输入名称' }]}
          >
            <Input placeholder="例如：pressure / app_logs" maxLength={64} />
          </Form.Item>
          <Form.Item name="description" label="描述">
            <TextArea placeholder="可选描述" maxLength={256} rows={3} showCount />
          </Form.Item>
        </Form>
      </FormDrawer>

      <DangerConfirmModal
        open={deleteTarget !== null}
        title={deleteTarget ? `删除文件类型：${deleteTarget.name}` : '删除文件类型'}
        content={
          <Text type="secondary">
            删除后该类型不再用于分类；已归类文件保留其 file_type 引用。此操作不可撤销。
          </Text>
        }
        confirmKeyword={deleteTarget?.name}
        loading={deleting}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={confirmDelete}
      />
    </div>
  )
}

export default FileTypesPage
