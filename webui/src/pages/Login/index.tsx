import { useState } from 'react'
import { Form, Input, Button, Typography, Alert } from 'antd'
import { UserOutlined, LockOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import useAuthStore from '../../store/auth'

const { Title } = Typography

interface LoginFormValues {
  username: string
  password: string
}

/**
 * Login page — username/password form that authenticates against /api/auth/login.
 * On success, redirects to /dashboard.
 */
function LoginPage() {
  const navigate = useNavigate()
  const login = useAuthStore((s) => s.login)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onFinish = async (values: LoginFormValues) => {
    setLoading(true)
    setError(null)
    try {
      await login(values.username, values.password)
      navigate('/dashboard', { replace: true })
    } catch (err: unknown) {
      const message =
        err instanceof Error ? err.message : '登录失败，请检查用户名和密码'
      setError(message)
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <Title level={3} style={{ textAlign: 'center', marginBottom: 24 }}>
        FileAgent 管理后台
      </Title>

      {error && (
        <Alert
          message={error}
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
        />
      )}

      <Form<LoginFormValues>
        name="login"
        onFinish={onFinish}
        autoComplete="off"
        size="large"
      >
        <Form.Item
          name="username"
          rules={[{ required: true, message: '请输入用户名' }]}
        >
          <Input prefix={<UserOutlined />} placeholder="用户名" />
        </Form.Item>

        <Form.Item
          name="password"
          rules={[{ required: true, message: '请输入密码' }]}
        >
          <Input.Password prefix={<LockOutlined />} placeholder="密码" />
        </Form.Item>

        <Form.Item style={{ marginBottom: 0 }}>
          <Button type="primary" htmlType="submit" loading={loading} block>
            登录
          </Button>
        </Form.Item>
      </Form>
    </>
  )
}

export default LoginPage
