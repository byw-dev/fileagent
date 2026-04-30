import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import apiClient from '../services/api'

/** User info returned from /api/auth/me */
export interface User {
  id: string
  username: string
  role: string
}

/** Auth store state */
export interface AuthState {
  accessToken: string | null
  refreshToken: string | null
  user: User | null
  isAuthenticated: boolean
}

/** Auth store actions */
export interface AuthActions {
  /** Login with username and password */
  login: (username: string, password: string) => Promise<void>
  /** Logout and clear tokens */
  logout: () => void
  /** Refresh access token using refresh token */
  refreshTokens: () => Promise<void>
  /** Directly set tokens (used by interceptor) */
  setTokens: (accessToken: string, refreshToken: string) => void
}

export type AuthStore = AuthState & AuthActions

const useAuthStore = create<AuthStore>()(
  persist(
    (set, get) => ({
      accessToken: null,
      refreshToken: null,
      user: null,
      isAuthenticated: false,

      login: async (username: string, password: string) => {
        const response = await apiClient.post<{
          access_token: string
          refresh_token: string
          user: User
        }>('/api/auth/login', { username, password })
        const { access_token, refresh_token, user } = response.data
        set({
          accessToken: access_token,
          refreshToken: refresh_token,
          user,
          isAuthenticated: true,
        })
      },

      logout: () => {
        const { accessToken } = get()
        if (accessToken) {
          apiClient.post('/api/auth/logout').catch(() => {})
        }
        set({
          accessToken: null,
          refreshToken: null,
          user: null,
          isAuthenticated: false,
        })
      },

      refreshTokens: async () => {
        const { refreshToken } = get()
        if (!refreshToken) throw new Error('No refresh token available')
        const response = await apiClient.post<{
          access_token: string
          refresh_token: string
        }>('/api/auth/refresh', { refresh_token: refreshToken })
        const { access_token, refresh_token: newRefreshToken } = response.data
        set({
          accessToken: access_token,
          refreshToken: newRefreshToken,
          isAuthenticated: true,
        })
      },

      setTokens: (accessToken: string, refreshToken: string) => {
        set({ accessToken, refreshToken, isAuthenticated: true })
      },
    }),
    {
      name: 'fileagent-auth',
      partialize: (state) => ({
        accessToken: state.accessToken,
        refreshToken: state.refreshToken,
        user: state.user,
        isAuthenticated: state.isAuthenticated,
      }),
    }
  )
)

export default useAuthStore
