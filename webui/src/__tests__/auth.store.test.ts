import { describe, it, expect, beforeEach, vi } from 'vitest'
import { act } from 'react'

// Reset store state between tests
beforeEach(() => {
  // Clear persisted localStorage state
  localStorage.clear()
})

describe('useAuthStore', () => {
  it('should have default unauthenticated state', async () => {
    // Dynamic import to pick up fresh module after localStorage.clear()
    const { default: useAuthStore } = await import('../store/auth')
    const state = useAuthStore.getState()
    expect(state.isAuthenticated).toBe(false)
    expect(state.accessToken).toBeNull()
    expect(state.refreshToken).toBeNull()
    expect(state.user).toBeNull()
  })

  it('should set tokens via setTokens action', async () => {
    const { default: useAuthStore } = await import('../store/auth')
    act(() => {
      useAuthStore.getState().setTokens('access-123', 'refresh-456')
    })
    const state = useAuthStore.getState()
    expect(state.accessToken).toBe('access-123')
    expect(state.refreshToken).toBe('refresh-456')
    expect(state.isAuthenticated).toBe(true)
  })

  it('should clear state on logout', async () => {
    const { default: useAuthStore } = await import('../store/auth')

    // Setup authenticated state
    act(() => {
      useAuthStore.getState().setTokens('access-123', 'refresh-456')
    })
    expect(useAuthStore.getState().isAuthenticated).toBe(true)

    // Logout
    act(() => {
      useAuthStore.getState().logout()
    })

    const state = useAuthStore.getState()
    expect(state.isAuthenticated).toBe(false)
    expect(state.accessToken).toBeNull()
    expect(state.refreshToken).toBeNull()
    expect(state.user).toBeNull()
  })

  it('should call /api/auth/login on login action', async () => {
    const { default: useAuthStore } = await import('../store/auth')

    // Mock the api client
    const mockPost = vi.fn().mockResolvedValue({
      data: {
        access_token: 'new-access',
        refresh_token: 'new-refresh',
        user: { id: 'u1', username: 'admin', role: 'super_admin' },
      },
    })

    // Patch the api module used inside auth store
    const apiModule = await import('../services/api')
    vi.spyOn(apiModule.default, 'post').mockImplementation(mockPost)

    await act(async () => {
      await useAuthStore.getState().login('admin', 'password')
    })

    expect(mockPost).toHaveBeenCalledWith('/api/auth/login', {
      username: 'admin',
      password: 'password',
    })

    const state = useAuthStore.getState()
    expect(state.isAuthenticated).toBe(true)
    expect(state.accessToken).toBe('new-access')
    expect(state.refreshToken).toBe('new-refresh')
    expect(state.user).toEqual({ id: 'u1', username: 'admin', role: 'super_admin' })

    vi.restoreAllMocks()
  })

  it('should throw on login failure', async () => {
    const { default: useAuthStore } = await import('../store/auth')

    const apiModule = await import('../services/api')
    vi.spyOn(apiModule.default, 'post').mockRejectedValue(new Error('Unauthorized'))

    await expect(
      act(async () => {
        await useAuthStore.getState().login('wrong', 'creds')
      })
    ).rejects.toThrow()

    vi.restoreAllMocks()
  })

  it('should call /api/auth/refresh on refreshTokens action', async () => {
    const { default: useAuthStore } = await import('../store/auth')

    act(() => {
      useAuthStore.getState().setTokens('old-access', 'old-refresh')
    })

    const mockPost = vi.fn().mockResolvedValue({
      data: { access_token: 'fresh-access', refresh_token: 'fresh-refresh' },
    })

    const apiModule = await import('../services/api')
    vi.spyOn(apiModule.default, 'post').mockImplementation(mockPost)

    await act(async () => {
      await useAuthStore.getState().refreshTokens()
    })

    expect(mockPost).toHaveBeenCalledWith('/api/auth/refresh', {
      refresh_token: 'old-refresh',
    })
    expect(useAuthStore.getState().accessToken).toBe('fresh-access')

    vi.restoreAllMocks()
  })

  it('should throw on refreshTokens when no refresh token', async () => {
    const { default: useAuthStore } = await import('../store/auth')

    // Ensure no refresh token
    act(() => {
      useAuthStore.setState({ refreshToken: null, accessToken: null, isAuthenticated: false })
    })

    await expect(
      act(async () => {
        await useAuthStore.getState().refreshTokens()
      })
    ).rejects.toThrow('No refresh token available')
  })
})

describe('useGlobalStore', () => {
  it('should manage loading state', async () => {
    const { default: useGlobalStore } = await import('../store/global')

    expect(useGlobalStore.getState().loading).toBe(false)

    act(() => {
      useGlobalStore.getState().setLoading(true)
    })
    expect(useGlobalStore.getState().loading).toBe(true)

    act(() => {
      useGlobalStore.getState().setLoading(false)
    })
    expect(useGlobalStore.getState().loading).toBe(false)
  })

  it('should add and remove notifications', async () => {
    const { default: useGlobalStore } = await import('../store/global')

    act(() => {
      useGlobalStore.getState().addNotification({ type: 'success', message: 'Done' })
    })

    const notifications = useGlobalStore.getState().notifications
    expect(notifications).toHaveLength(1)
    expect(notifications[0].message).toBe('Done')
    expect(notifications[0].id).toBeTruthy()

    const id = notifications[0].id
    act(() => {
      useGlobalStore.getState().removeNotification(id)
    })

    expect(useGlobalStore.getState().notifications).toHaveLength(0)
  })

  it('should clear all notifications', async () => {
    const { default: useGlobalStore } = await import('../store/global')

    act(() => {
      useGlobalStore.getState().addNotification({ type: 'error', message: 'Error 1' })
      useGlobalStore.getState().addNotification({ type: 'warning', message: 'Warn 1' })
    })

    expect(useGlobalStore.getState().notifications.length).toBeGreaterThan(0)

    act(() => {
      useGlobalStore.getState().clearNotifications()
    })

    expect(useGlobalStore.getState().notifications).toHaveLength(0)
  })
})
