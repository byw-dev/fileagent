import { useEffect, useRef, useState } from 'react'
import useSWR from 'swr'
import { App, Button, Drawer, Form, Input, Modal, Space, Switch, Tag, Typography } from 'antd'
import { PlusOutlined, TagsOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import useAuthStore from '../../store/auth'
import {
  listTagKeys,
  createTagKey,
  updateTagKey,
  deleteTagKey,
  listTagValues,
  createTagValue,
  deleteTagValue,
} from '../../services/tags'
import type { TagKey, TagValue } from '../../services/tags'

const { Title, Text } = Typography

/** Form shape for creating/editing a tag key. */
interface KeyFormValues {
  key: string
  label: string
  value_controlled: boolean
  required_at_collection: boolean
  allow_path_var: boolean
}

/**
 * Tag vocabulary management (metadata 6c, screen 7c). Lists controlled tag keys
 * and, per key, their legal values. Reads are open to any authenticated user;
 * create/edit/delete are super_admin only (also enforced by the API).
 */
function TagKeysPage() {
  const user = useAuthStore((s) => s.user)
  const isSuperAdmin = user?.role === 'super_admin'
  const { modal, message } = App.useApp()
  const actionRef = useRef<ActionType | undefined>(undefined)

  const [keyModalOpen, setKeyModalOpen] = useState(false)
  const [editing, setEditing] = useState<TagKey | null>(null)
  const [keyForm] = Form.useForm<KeyFormValues>()
  const [submitting, setSubmitting] = useState(false)

  const [valuesKey, setValuesKey] = useState<TagKey | null>(null)

  const openCreate = () => {
    setEditing(null)
    keyForm.setFieldsValue({
      key: '',
      label: '',
      value_controlled: true,
      required_at_collection: false,
      allow_path_var: true,
    })
    setKeyModalOpen(true)
  }

  const openEdit = (k: TagKey) => {
    setEditing(k)
    keyForm.setFieldsValue({
      key: k.key,
      label: k.label,
      value_controlled: k.value_controlled,
      required_at_collection: k.required_at_collection,
      allow_path_var: k.allow_path_var,
    })
    setKeyModalOpen(true)
  }

  const submitKey = async () => {
    const values = await keyForm.validateFields()
    setSubmitting(true)
    try {
      if (editing) {
        await updateTagKey(editing.key, {
          label: values.label,
          value_controlled: values.value_controlled,
          required_at_collection: values.required_at_collection,
          allow_path_var: values.allow_path_var,
        })
        message.success('已更新标签键')
      } else {
        await createTagKey(values)
        message.success('已创建标签键')
      }
      setKeyModalOpen(false)
      actionRef.current?.reload()
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status
      message.error(status === 409 ? '标签键已存在' : '保存失败')
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = (k: TagKey) => {
    modal.confirm({
      title: `删除标签键：${k.key}`,
      content: '删除后不影响已打标文件，但该键将不能再用于采集与筛选。确认删除？',
      okType: 'danger',
      onOk: async () => {
        try {
          await deleteTagKey(k.key)
          message.success('已删除')
          actionRef.current?.reload()
        } catch {
          message.error('删除失败')
        }
      },
    })
  }

  const columns: ProColumns<TagKey>[] = [
    {
      title: '键',
      dataIndex: 'key',
      key: 'key',
      render: (_, k) => <Text code>{k.key}</Text>,
    },
    { title: '名称', dataIndex: 'label', key: 'label' },
    {
      title: '取值',
      dataIndex: 'value_controlled',
      key: 'value_controlled',
      width: 90,
      render: (_, k) => (k.value_controlled ? <Tag color="blue">受控</Tag> : <Tag>自由</Tag>),
    },
    {
      title: '采集必填',
      dataIndex: 'required_at_collection',
      key: 'required_at_collection',
      width: 90,
      render: (_, k) => (k.required_at_collection ? <Tag color="orange">是</Tag> : <Tag>否</Tag>),
    },
    {
      title: '路径变量',
      dataIndex: 'allow_path_var',
      key: 'allow_path_var',
      width: 90,
      render: (_, k) => (k.allow_path_var ? <Tag color="green">允许</Tag> : <Tag>否</Tag>),
    },
    {
      title: '',
      key: 'reserved',
      width: 70,
      render: (_, k) => (k.system_reserved ? <Tag color="purple">系统</Tag> : null),
    },
    {
      title: '操作',
      key: 'action',
      width: 220,
      render: (_, k) => (
        <Space size="small">
          {k.value_controlled && (
            <Button size="small" icon={<TagsOutlined />} onClick={() => setValuesKey(k)}>
              取值
            </Button>
          )}
          {isSuperAdmin && (
            <Button size="small" onClick={() => openEdit(k)}>
              编辑
            </Button>
          )}
          {isSuperAdmin && !k.system_reserved && (
            <Button size="small" danger onClick={() => handleDelete(k)}>
              删除
            </Button>
          )}
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>
          标签词表
        </Title>
        {isSuperAdmin && (
          <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
            新建标签键
          </Button>
        )}
      </Space>

      <ProTable<TagKey>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={false}
        options={false}
        request={async () => {
          try {
            const data = await listTagKeys()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      <Modal
        open={keyModalOpen}
        title={editing ? `编辑标签键：${editing.key}` : '新建标签键'}
        confirmLoading={submitting}
        onOk={submitKey}
        onCancel={() => setKeyModalOpen(false)}
        destroyOnHidden
      >
        <Form form={keyForm} layout="vertical" preserve={false}>
          <Form.Item
            name="key"
            label="键"
            rules={[
              { required: true, message: '请输入键名' },
              { pattern: /^[a-z][a-z0-9_]*$/, message: '仅小写字母、数字、下划线，且以字母开头' },
            ]}
            extra="创建后不可修改，例如 site / vendor / level"
          >
            <Input placeholder="site" disabled={!!editing} />
          </Form.Item>
          <Form.Item name="label" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
            <Input placeholder="站点" />
          </Form.Item>
          <Form.Item
            name="value_controlled"
            label="取值受控"
            valuePropName="checked"
            extra="开启后取值必须在词表内"
          >
            <Switch />
          </Form.Item>
          <Form.Item name="required_at_collection" label="采集必填" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="allow_path_var" label="允许路径变量映射" valuePropName="checked">
            <Switch />
          </Form.Item>
        </Form>
      </Modal>

      <ValuesDrawer tagKey={valuesKey} canWrite={isSuperAdmin} onClose={() => setValuesKey(null)} />
    </div>
  )
}

/** Drawer that lists and edits a key's controlled values. The body is a separate
 * component keyed by the tag key, so opening a different key remounts it with a
 * clean slate — no stale values flash and no state is set during render. */
function ValuesDrawer({
  tagKey,
  canWrite,
  onClose,
}: {
  tagKey: TagKey | null
  canWrite: boolean
  onClose: () => void
}) {
  return (
    <Drawer title={tagKey ? `取值：${tagKey.key}` : '取值'} open={tagKey !== null} onClose={onClose} width={420}>
      {tagKey && <ValuesPanel key={tagKey.key} keyName={tagKey.key} canWrite={canWrite} />}
    </Drawer>
  )
}

/** The values list + editor for a single tag key. Mounted fresh per key. Data is
 * fetched via SWR (the project HTTP pattern) so there is no manual state sync. */
function ValuesPanel({ keyName, canWrite }: { keyName: string; canWrite: boolean }) {
  const { message } = App.useApp()
  const {
    data: values = [],
    isLoading: loading,
    mutate,
    error,
  } = useSWR(['tag-values', keyName], () => listTagValues(keyName))
  const [newValue, setNewValue] = useState('')
  const [adding, setAdding] = useState(false)

  useEffect(() => {
    if (error) message.error('加载取值失败')
  }, [error, message])

  const add = async () => {
    const v = newValue.trim()
    if (!v) return
    setAdding(true)
    try {
      await createTagValue(keyName, v)
      setNewValue('')
      await mutate()
    } catch (err) {
      const status = (err as { response?: { status?: number } }).response?.status
      message.error(status === 409 ? '取值已存在' : '添加失败')
    } finally {
      setAdding(false)
    }
  }

  const remove = async (val: TagValue) => {
    try {
      await deleteTagValue(keyName, val.id)
      await mutate()
    } catch {
      message.error('删除失败')
    }
  }

  return (
    <>
      {canWrite && (
        <Space.Compact style={{ width: '100%', marginBottom: 16 }}>
          <Input
            placeholder="新增取值，例如 tokyo"
            value={newValue}
            onChange={(e) => setNewValue(e.target.value)}
            onPressEnter={add}
          />
          <Button type="primary" loading={adding} onClick={add}>
            添加
          </Button>
        </Space.Compact>
      )}
      {values.length === 0 && !loading ? (
        <Text type="secondary">暂无取值</Text>
      ) : (
        <Space size={[8, 8]} wrap>
          {values.map((v) => (
            <Tag
              key={v.id}
              closable={canWrite}
              onClose={(e) => {
                e.preventDefault()
                void remove(v)
              }}
            >
              {v.value}
            </Tag>
          ))}
        </Space>
      )}
    </>
  )
}

export default TagKeysPage
