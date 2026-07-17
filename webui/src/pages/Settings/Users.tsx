import { useRef, useState } from 'react'
import { App, Button, Form, Input, Select, Space, Tag, Typography } from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import {
  listUsers,
  createUser,
  updateUser,
  setUserActive,
  updateUserPassword,
} from '../../services/users'
import type { ManagedUser } from '../../services/users'
import useAuthStore from '../../store/auth'
import FormDrawer from '../../components/FormDrawer'
import StatusBadge from '../../components/StatusBadge'
import TimeText from '../../components/TimeText'
import EmptyState from '../../components/EmptyState'
import { useDangerConfirm } from '../../hooks/useDangerConfirm'

const { Title } = Typography

const ROLE_OPTIONS = [
  { label: '超级管理员', value: 'super_admin' },
  { label: '组织管理员', value: 'org_admin' },
  { label: '只读成员', value: 'org_viewer' },
]

const ROLE_COLOR: Record<string, string> = {
  super_admin: 'red',
  org_admin: 'blue',
  org_viewer: 'green',
}

const ROLE_LABEL: Record<string, string> = {
  super_admin: '超级管理员',
  org_admin: '组织管理员',
  org_viewer: '只读成员',
}

/**
 * User management page (super_admin only, 5b/5c). Create/edit/reset-password run
 * in the site-wide drawer (交互定则 1); users are disabled/enabled rather than
 * hard-deleted (5b, danger confirm); MM-DD HH:mm times.
 */
function SettingsUsersPage() {
  const currentUser = useAuthStore((s) => s.user)
  const isSuperAdmin = currentUser?.role === 'super_admin'
  const { message } = App.useApp()
  const dangerConfirm = useDangerConfirm()
  const actionRef = useRef<ActionType | undefined>(undefined)

  const [createOpen, setCreateOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<ManagedUser | null>(null)
  const [pwTarget, setPwTarget] = useState<ManagedUser | null>(null)
  const [submitting, setSubmitting] = useState(false)

  const [createForm] = Form.useForm()
  const [editForm] = Form.useForm()
  const [pwForm] = Form.useForm()

  if (!isSuperAdmin) {
    return (
      <div>
        <Title level={3}>用户管理</Title>
        <Typography.Text type="secondary">仅超级管理员可访问此页面。</Typography.Text>
      </div>
    )
  }

  const submitCreate = async () => {
    let values: { username: string; email?: string; password: string; role: string }
    try {
      values = await createForm.validateFields()
    } catch {
      return
    }
    setSubmitting(true)
    try {
      await createUser(values)
      message.success('用户创建成功')
      setCreateOpen(false)
      createForm.resetFields()
      actionRef.current?.reload()
    } catch {
      message.error('创建失败')
    } finally {
      setSubmitting(false)
    }
  }

  const submitEdit = async () => {
    if (!editTarget) return
    let values: { username?: string; email?: string; role?: string }
    try {
      values = await editForm.validateFields()
    } catch {
      return
    }
    setSubmitting(true)
    try {
      await updateUser(editTarget.id, values)
      message.success('更新成功')
      setEditTarget(null)
      actionRef.current?.reload()
    } catch {
      message.error('更新失败')
    } finally {
      setSubmitting(false)
    }
  }

  const submitPassword = async () => {
    if (!pwTarget) return
    let values: { password: string; confirm: string }
    try {
      values = await pwForm.validateFields()
    } catch {
      return
    }
    if (values.password !== values.confirm) {
      message.error('两次密码输入不一致')
      return
    }
    setSubmitting(true)
    try {
      await updateUserPassword(pwTarget.id, values.password)
      message.success('密码已更新')
      setPwTarget(null)
      pwForm.resetFields()
    } catch {
      message.error('密码修改失败')
    } finally {
      setSubmitting(false)
    }
  }

  const toggleActive = (u: ManagedUser) => {
    if (u.is_active) {
      dangerConfirm({
        title: `禁用用户：${u.username}`,
        content: '禁用后该用户无法登录，但账号与历史记录保留，可随时重新启用。',
        okText: '禁用',
        onOk: async () => {
          try {
            await setUserActive(u.id, false)
            message.success('已禁用')
            actionRef.current?.reload()
          } catch {
            message.error('操作失败')
          }
        },
      })
    } else {
      void (async () => {
        try {
          await setUserActive(u.id, true)
          message.success('已启用')
          actionRef.current?.reload()
        } catch {
          message.error('操作失败')
        }
      })()
    }
  }

  const columns: ProColumns<ManagedUser>[] = [
    { title: '用户名', dataIndex: 'username', key: 'username' },
    {
      title: '邮箱',
      dataIndex: 'email',
      key: 'email',
      render: (_, u) => u.email ?? '—',
    },
    {
      title: '角色',
      dataIndex: 'role',
      key: 'role',
      width: 130,
      render: (_, u) => <Tag color={ROLE_COLOR[u.role] ?? 'default'}>{ROLE_LABEL[u.role] ?? u.role}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'is_active',
      key: 'is_active',
      width: 90,
      render: (_, u) => <StatusBadge status={u.is_active ? 'ACTIVE' : 'INACTIVE'} domain="user" />,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 150,
      render: (_, u) => <TimeText value={u.created_at} />,
    },
    {
      title: '操作',
      key: 'action',
      width: 170,
      align: 'right',
      render: (_, u) => (
        <Space size="middle">
          <Button
            type="link"
            style={{ padding: 0, height: 'auto' }}
            onClick={() => {
              editForm.setFieldsValue({ username: u.username, email: u.email, role: u.role })
              setEditTarget(u)
            }}
          >
            编辑
          </Button>
          <Button type="link" style={{ padding: 0, height: 'auto' }} onClick={() => setPwTarget(u)}>
            改密
          </Button>
          <Button
            type="link"
            danger={u.is_active}
            style={{ padding: 0, height: 'auto' }}
            disabled={u.id === currentUser?.id}
            onClick={() => toggleActive(u)}
          >
            {u.is_active ? '禁用' : '启用'}
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>用户管理</Title>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>
          新建用户
        </Button>
      </Space>

      <ProTable<ManagedUser>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        options={false}
        pagination={{ pageSize: 20, hideOnSinglePage: true }}
        locale={{ emptyText: <EmptyState description="还没有用户" /> }}
        request={async () => {
          try {
            const data = await listUsers()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      {/* Create user drawer (5c) */}
      <FormDrawer
        open={createOpen}
        title="新建用户"
        onClose={() => setCreateOpen(false)}
        onSubmit={submitCreate}
        loading={submitting}
        submitText="创建"
      >
        <Form form={createForm} layout="vertical" preserve={false} onFinish={submitCreate}>
          <Form.Item label="用户名" name="username" rules={[{ required: true, message: '请输入用户名' }]}>
            <Input maxLength={32} />
          </Form.Item>
          <Form.Item label="邮箱" name="email">
            <Input type="email" maxLength={128} />
          </Form.Item>
          <Form.Item
            label="初始密码"
            name="password"
            rules={[{ required: true, min: 8, message: '密码至少 8 位' }]}
            extra="初始密码由管理员设置，请一次性妥善告知用户，用户可自行修改。"
          >
            <Input.Password />
          </Form.Item>
          <Form.Item label="角色" name="role" rules={[{ required: true, message: '请选择角色' }]}>
            <Select options={ROLE_OPTIONS} />
          </Form.Item>
        </Form>
      </FormDrawer>

      {/* Edit user drawer */}
      <FormDrawer
        open={!!editTarget}
        title={`编辑用户：${editTarget?.username ?? ''}`}
        onClose={() => setEditTarget(null)}
        onSubmit={submitEdit}
        loading={submitting}
      >
        <Form form={editForm} layout="vertical" preserve={false} onFinish={submitEdit}>
          <Form.Item label="用户名" name="username">
            <Input maxLength={32} />
          </Form.Item>
          <Form.Item label="邮箱" name="email">
            <Input type="email" maxLength={128} />
          </Form.Item>
          <Form.Item label="角色" name="role">
            <Select options={ROLE_OPTIONS} />
          </Form.Item>
        </Form>
      </FormDrawer>

      {/* Reset password drawer */}
      <FormDrawer
        open={!!pwTarget}
        title={`修改密码：${pwTarget?.username ?? ''}`}
        onClose={() => setPwTarget(null)}
        onSubmit={submitPassword}
        loading={submitting}
        submitText="确认修改"
      >
        <Form form={pwForm} layout="vertical" preserve={false} onFinish={submitPassword}>
          <Form.Item label="新密码" name="password" rules={[{ required: true, min: 8, message: '密码至少 8 位' }]}>
            <Input.Password />
          </Form.Item>
          <Form.Item label="确认密码" name="confirm" rules={[{ required: true, message: '请再次输入密码' }]}>
            <Input.Password />
          </Form.Item>
        </Form>
      </FormDrawer>
    </div>
  )
}

export default SettingsUsersPage
