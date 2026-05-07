import { useState } from 'react'
import {
  Button,
  Card,
  Descriptions,
  Divider,
  Form,
  Input,
  Space,
  Tag,
  Typography,
  message,
} from 'antd'
import useAuthStore from '../../store/auth'
import { updateUserPassword } from '../../services/users'

const { Title } = Typography

const ROLE_COLOR: Record<string, string> = {
  super_admin: 'red',
  org_admin: 'blue',
  org_viewer: 'green',
}

/**
 * Profile settings page — shows current user info and allows password change.
 */
function ProfilePage() {
  const user = useAuthStore((s) => s.user)
  const [form] = Form.useForm()
  const [submitting, setSubmitting] = useState(false)

  const handlePasswordChange = async (values: {
    password: string
    confirm: string
  }) => {
    if (values.password !== values.confirm) {
      message.error('两次密码输入不一致')
      return
    }
    if (!user) return

    setSubmitting(true)
    try {
      await updateUserPassword(user.id, values.password)
      message.success('密码修改成功')
      form.resetFields()
    } catch {
      message.error('密码修改失败，请重试')
    } finally {
      setSubmitting(false)
    }
  }

  if (!user) {
    return <Typography.Text type="secondary">未登录</Typography.Text>
  }

  return (
    <div style={{ maxWidth: 640 }}>
      <Title level={3} style={{ marginBottom: 16 }}>个人信息</Title>

      <Card style={{ marginBottom: 24 }}>
        <Descriptions column={1} bordered>
          <Descriptions.Item label="用户名">{user.username}</Descriptions.Item>
          <Descriptions.Item label="用户 ID">{user.id}</Descriptions.Item>
          <Descriptions.Item label="角色">
            <Tag color={ROLE_COLOR[user.role] ?? 'default'}>{user.role}</Tag>
          </Descriptions.Item>
        </Descriptions>
      </Card>

      <Divider orientation="left">修改密码</Divider>

      <Card>
        <Form form={form} layout="vertical" onFinish={handlePasswordChange}>
          <Form.Item
            label="新密码"
            name="password"
            rules={[{ required: true, min: 8, message: '密码至少 8 位' }]}
          >
            <Input.Password placeholder="至少 8 位字符" />
          </Form.Item>

          <Form.Item
            label="确认密码"
            name="confirm"
            dependencies={['password']}
            rules={[
              { required: true, message: '请确认新密码' },
              ({ getFieldValue }) => ({
                validator(_, value) {
                  if (!value || getFieldValue('password') === value) {
                    return Promise.resolve()
                  }
                  return Promise.reject(new Error('两次密码输入不一致'))
                },
              }),
            ]}
          >
            <Input.Password placeholder="再次输入新密码" />
          </Form.Item>

          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit" loading={submitting}>
                修改密码
              </Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}

export default ProfilePage
