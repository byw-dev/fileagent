import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import type { ManagedUser } from '../services/users'
import ProfilePage from '../pages/Settings/Profile'
import SettingsUsersPage from '../pages/Settings/Users'

const listUsersMock = vi.fn()
const updateUserPasswordMock = vi.fn()

let currentUser: { id: string; username: string; role: string } | null = null

vi.mock('../store/auth', () => ({
  default: (selector: (state: { user: typeof currentUser }) => unknown) =>
    selector({ user: currentUser }),
}))

vi.mock('../services/users', () => ({
  listUsers: () => listUsersMock(),
  createUser: vi.fn(),
  updateUser: vi.fn(),
  deleteUser: vi.fn(),
  updateUserPassword: (...args: unknown[]) => updateUserPasswordMock(...args),
}))

vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')

  return {
    ProTable: ({
      request,
    }: {
      request?: () => Promise<{ data?: ManagedUser[] }>
    }) => {
      const requestedRef = ReactModule.useRef(false)

      if (!requestedRef.current) {
        requestedRef.current = true
        void request?.()
      }

      return <div data-testid="mock-pro-table" />
    },
  }
})

describe('ProfilePage', () => {
  beforeEach(() => {
    currentUser = null
    listUsersMock.mockReset()
    updateUserPasswordMock.mockReset()
  })

  it('renders logged-in user details and hides the logged-out message', () => {
    currentUser = { id: 'u-1', username: 'admin', role: 'super_admin' }

    render(<ProfilePage />)

    expect(screen.queryByText('未登录')).not.toBeInTheDocument()
    expect(screen.getByText('admin')).toBeInTheDocument()
    expect(screen.getByText('super_admin')).toBeInTheDocument()
  })
})

describe('SettingsUsersPage', () => {
  beforeEach(() => {
    currentUser = null
    listUsersMock.mockReset()
    updateUserPasswordMock.mockReset()
  })

  it('renders the full user management UI for super admins', async () => {
    currentUser = { id: 'u-1', username: 'admin', role: 'super_admin' }
    listUsersMock.mockResolvedValue([
      {
        id: 'u-2',
        org_id: 'org-1',
        username: 'operator',
        email: 'operator@example.com',
        role: 'org_viewer',
        is_active: true,
        created_at: '2026-05-07T00:00:00Z',
      },
    ])

    render(<SettingsUsersPage />)

    expect(screen.getByText('用户管理')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /新建用户/ })).toBeInTheDocument()

    await waitFor(() => {
      expect(listUsersMock).toHaveBeenCalledTimes(1)
    })
    expect(screen.getByTestId('mock-pro-table')).toBeInTheDocument()
  })

  it('shows the access denied message for non-super admins', () => {
    currentUser = { id: 'u-3', username: 'viewer', role: 'org_viewer' }

    render(<SettingsUsersPage />)

    expect(screen.getByText('仅超级管理员可访问此页面。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /新建用户/ })).not.toBeInTheDocument()
  })
})
