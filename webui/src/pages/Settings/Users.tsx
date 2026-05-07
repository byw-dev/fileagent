import { useState } from 'react'
import {
  Button,
  Form,
  Input,
  Modal,
  Select,
  Space,
  Tag,
  Typography,
  message,
} from 'antd'
import { PlusOutlined, EditOutlined, DeleteOutlined, KeyOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useRef } from 'react'
import {
  listUsers,
  createUser,
  updateUser,
  deleteUser,
  updateUserPassword,
} from '../../services/users'
import type { ManagedUser } from '../../services/users'
import useAuthStore from '../../store/auth'

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

/**
 * User management page (super_admin only) — list, create, edit, delete users
 * and reset their passwords.
 */
function SettingsUsersPage() {
  const currentUser = useAuthStore((s) => s.user)
  const isSuperAdmin = currentUser?.role === 'super_admin'

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

  const handleCreate = async (values: {
    username: string
    email?: string
    password: string
    role: string
  }) => {
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

  const handleEdit = async (values: { username?: string; email?: string; role?: string }) => {
    if (!editTarget) return
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

  const handleDelete = (user: ManagedUser) => {
    Modal.confirm({
      title: `删除用户：${user.username}`,
      content: '确认删除该用户？此操作不可撤销。',
      okType: 'danger',
      onOk: async () => {
        try {
          await deleteUser(user.id)
          message.success('用户已删除')
          actionRef.current?.reload()
        } catch {
          message.error('删除失败')
        }
      },
    })
  }

  const handlePasswordChange = async (values: { password: string; confirm: string }) => {
    if (!pwTarget) return
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

  const columns: ProColumns<ManagedUser>[] = [
    {
      title: '用户名',
      dataIndex: 'username',
      key: 'username',
    },
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
      render: (_, u) => (
        <Tag color={ROLE_COLOR[u.role] ?? 'default'}>{u.role}</Tag>
      ),
    },
    {
      title: '状态',
      dataIndex: 'is_active',
      key: 'is_active',
      width: 80,
      render: (_, u) => (
        <Tag color={u.is_active ? 'green' : 'red'}>{u.is_active ? '活跃' : '禁用'}</Tag>
      ),
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (_, u) => new Date(u.created_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 200,
      render: (_, u) => (
        <Space>
          <Button
            size="small"
            icon={<EditOutlined />}
            onClick={() => {
              editForm.setFieldsValue({ username: u.username, email: u.email, role: u.role })
              setEditTarget(u)
            }}
          >
            编辑
          </Button>
          <Button
            size="small"
            icon={<KeyOutlined />}
            onClick={() => setPwTarget(u)}
          >
            改密
          </Button>
          <Button
            size="small"
            danger
            icon={<DeleteOutlined />}
            disabled={u.id === currentUser?.id}
            onClick={() => handleDelete(u)}
          >
            删除
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>用户管理</Title>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => setCreateOpen(true)}
        >
          新建用户
        </Button>
      </Space>

      <ProTable<ManagedUser>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        request={async () => {
          try {
            const data = await listUsers()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />

      {/* Create user modal */}
      <Modal
        title="新建用户"
        open={createOpen}
        onCancel={() => { setCreateOpen(false); createForm.resetFields() }}
        onOk={() => createForm.submit()}
        confirmLoading={submitting}
        okText="创建"
        cancelText="取消"
      >
        <Form form={createForm} layout="vertical" onFinish={handleCreate}>
          <Form.Item
            label="用户名"
            name="username"
            rules={[{ required: true, message: '请输入用户名' }]}
          >
            <Input maxLength={32} />
          </Form.Item>
          <Form.Item label="邮箱" name="email">
            <Input type="email" maxLength={128} />
          </Form.Item>
          <Form.Item
            label="密码"
            name="password"
            rules={[{ required: true, min: 8, message: '密码至少 8 位' }]}
          >
            <Input.Password />
          </Form.Item>
          <Form.Item
            label="角色"
            name="role"
            rules={[{ required: true, message: '请选择角色' }]}
          >
            <Select options={ROLE_OPTIONS} />
          </Form.Item>
        </Form>
      </Modal>

      {/* Edit user modal */}
      <Modal
        title={`编辑用户：${editTarget?.username}`}
        open={!!editTarget}
        onCancel={() => setEditTarget(null)}
        onOk={() => editForm.submit()}
        confirmLoading={submitting}
        okText="保存"
        cancelText="取消"
      >
        <Form form={editForm} layout="vertical" onFinish={handleEdit}>
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
      </Modal>

      {/* Change password modal */}
      <Modal
        title={`修改密码：${pwTarget?.username}`}
        open={!!pwTarget}
        onCancel={() => { setPwTarget(null); pwForm.resetFields() }}
        onOk={() => pwForm.submit()}
        confirmLoading={submitting}
        okText="确认修改"
        cancelText="取消"
      >
        <Form form={pwForm} layout="vertical" onFinish={handlePasswordChange}>
          <Form.Item
            label="新密码"
            name="password"
            rules={[{ required: true, min: 8, message: '密码至少 8 位' }]}
          >
            <Input.Password />
          </Form.Item>
          <Form.Item
            label="确认密码"
            name="confirm"
            rules={[{ required: true, message: '请再次输入密码' }]}
          >
            <Input.Password />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

export default SettingsUsersPage
