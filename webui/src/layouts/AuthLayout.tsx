import { Card } from 'antd'
import { Outlet } from 'react-router-dom'

/**
 * AuthLayout — centered card layout used for login and other auth pages.
 */
function AuthLayout() {
  return (
    <div
      style={{
        display: 'flex',
        justifyContent: 'center',
        alignItems: 'center',
        minHeight: '100vh',
        background: '#f0f2f5',
      }}
    >
      <Card style={{ width: 400, boxShadow: '0 4px 24px rgba(0,0,0,0.08)' }}>
        <Outlet />
      </Card>
    </div>
  )
}

export default AuthLayout
