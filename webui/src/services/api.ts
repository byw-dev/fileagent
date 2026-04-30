import axios, {
  AxiosInstance,
  AxiosRequestConfig,
  InternalAxiosRequestConfig,
} from 'axios'

/** Queued request item for retry after token refresh */
interface QueuedRequest {
  resolve: (token: string) => void
  reject: (error: unknown) => void
}

let isRefreshing = false
let requestQueue: QueuedRequest[] = []

/**
 * Process all queued requests after a token refresh attempt.
 * @param error - If set, rejects all queued requests; otherwise resolves with new token.
 * @param token - New access token on success.
 */
function processQueue(error: unknown, token: string | null = null): void {
  requestQueue.forEach((prom) => {
    if (error) {
      prom.reject(error)
    } else {
      prom.resolve(token!)
    }
  })
  requestQueue = []
}

/**
 * Axios instance pre-configured for the FileAgent API.
 * Base URL is read from the VITE_API_BASE_URL environment variable (default: '/').
 */
const apiClient: AxiosInstance = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL ?? '/',
  headers: {
    'Content-Type': 'application/json',
  },
})

// Request interceptor: inject Authorization header
apiClient.interceptors.request.use(
  (config: InternalAxiosRequestConfig) => {
    const stored = localStorage.getItem('fileagent-auth')
    if (stored) {
      try {
        const parsed = JSON.parse(stored)
        const token = parsed?.state?.accessToken
        if (token && config.headers) {
          config.headers['Authorization'] = `Bearer ${token}`
        }
      } catch {
        // ignore parse errors
      }
    }
    return config
  },
  (error) => Promise.reject(error)
)

// Response interceptor: handle 401 with token refresh and queue
apiClient.interceptors.response.use(
  (response) => response,
  async (error) => {
    const originalRequest = error.config as AxiosRequestConfig & {
      _retry?: boolean
    }

    if (error.response?.status === 401 && !originalRequest._retry) {
      if (isRefreshing) {
        return new Promise((resolve, reject) => {
          requestQueue.push({
            resolve: (token: string) => {
              if (originalRequest.headers) {
                originalRequest.headers['Authorization'] = `Bearer ${token}`
              } else {
                originalRequest.headers = { Authorization: `Bearer ${token}` }
              }
              resolve(apiClient(originalRequest))
            },
            reject,
          })
        })
      }

      originalRequest._retry = true
      isRefreshing = true

      try {
        const stored = localStorage.getItem('fileagent-auth')
        let refreshToken: string | null = null
        if (stored) {
          const parsed = JSON.parse(stored)
          refreshToken = parsed?.state?.refreshToken ?? null
        }

        if (!refreshToken) {
          throw new Error('No refresh token')
        }

        const response = await axios.post<{
          access_token: string
          refresh_token: string
        }>(
          `${import.meta.env.VITE_API_BASE_URL ?? ''}/api/auth/refresh`,
          { refresh_token: refreshToken },
          { headers: { 'Content-Type': 'application/json' } }
        )

        const { access_token, refresh_token: newRefreshToken } = response.data

        if (stored) {
          const parsed = JSON.parse(stored)
          parsed.state.accessToken = access_token
          parsed.state.refreshToken = newRefreshToken
          parsed.state.isAuthenticated = true
          localStorage.setItem('fileagent-auth', JSON.stringify(parsed))
        }

        processQueue(null, access_token)

        if (originalRequest.headers) {
          originalRequest.headers['Authorization'] = `Bearer ${access_token}`
        } else {
          originalRequest.headers = {
            Authorization: `Bearer ${access_token}`,
          }
        }

        return apiClient(originalRequest)
      } catch (refreshError) {
        processQueue(refreshError, null)

        localStorage.removeItem('fileagent-auth')
        window.location.href = '/login'

        return Promise.reject(refreshError)
      } finally {
        isRefreshing = false
      }
    }

    return Promise.reject(error)
  }
)

export default apiClient
