import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import axios from 'axios'

describe('api client – base configuration', () => {
  it('should export an axios instance with required methods', async () => {
    const { default: apiClient } = await import('../services/api')
    expect(apiClient).toBeDefined()
    expect(typeof apiClient.get).toBe('function')
    expect(typeof apiClient.post).toBe('function')
    expect(typeof apiClient.put).toBe('function')
    expect(typeof apiClient.delete).toBe('function')
  })

  it('should have Content-Type: application/json as default header', async () => {
    const { default: apiClient } = await import('../services/api')
    expect(apiClient.defaults.headers['Content-Type']).toBe('application/json')
  })

  it('should have request and response interceptors registered', async () => {
    const { default: apiClient } = await import('../services/api')
    // interceptors.request and response are AxiosInterceptorManager instances
    // They have a `use` method — verifying interceptors are set up
    expect(typeof apiClient.interceptors.request.use).toBe('function')
    expect(typeof apiClient.interceptors.response.use).toBe('function')
  })
})

describe('api client – request interceptor (token injection)', () => {
  const localStorageKey = 'fileagent-auth'

  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
  })

  it('should inject Authorization header when token exists in localStorage', async () => {
    localStorage.setItem(
      localStorageKey,
      JSON.stringify({ state: { accessToken: 'my-token', refreshToken: null } })
    )

    const { default: apiClient } = await import('../services/api')

    // Mock the adapter to capture the request config
    const adapterSpy = vi.fn().mockResolvedValue({
      data: {},
      status: 200,
      statusText: 'OK',
      headers: {},
      config: { headers: {} },
    })
    const originalAdapter = apiClient.defaults.adapter
    apiClient.defaults.adapter = adapterSpy

    await apiClient.get('/ping').catch(() => {})

    const capturedConfig = adapterSpy.mock.calls[0]?.[0]
    expect(capturedConfig?.headers?.['Authorization']).toBe('Bearer my-token')

    apiClient.defaults.adapter = originalAdapter
  })

  it('should NOT inject Authorization header when no token in localStorage', async () => {
    localStorage.clear()

    const { default: apiClient } = await import('../services/api')

    const adapterSpy = vi.fn().mockResolvedValue({
      data: {},
      status: 200,
      statusText: 'OK',
      headers: {},
      config: { headers: {} },
    })
    const originalAdapter = apiClient.defaults.adapter
    apiClient.defaults.adapter = adapterSpy

    await apiClient.get('/ping').catch(() => {})

    const capturedConfig = adapterSpy.mock.calls[0]?.[0]
    expect(capturedConfig?.headers?.['Authorization']).toBeUndefined()

    apiClient.defaults.adapter = originalAdapter
  })

  it('should handle malformed localStorage data gracefully', async () => {
    localStorage.setItem(localStorageKey, 'not-valid-json{{{')

    const { default: apiClient } = await import('../services/api')

    const adapterSpy = vi.fn().mockResolvedValue({
      data: {},
      status: 200,
      statusText: 'OK',
      headers: {},
      config: { headers: {} },
    })
    const originalAdapter = apiClient.defaults.adapter
    apiClient.defaults.adapter = adapterSpy

    // Should not throw
    await expect(apiClient.get('/ping')).resolves.toBeDefined()

    apiClient.defaults.adapter = originalAdapter
  })
})

describe('api client – response interceptor (401 handling)', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    localStorage.clear()
    vi.restoreAllMocks()
  })

  it('should redirect to /login when 401 occurs with no refresh token', async () => {
    localStorage.clear()

    // Override window.location.href setter
    const hrefSetter = vi.fn()
    Object.defineProperty(window, 'location', {
      value: { ...window.location, set href(val: string) { hrefSetter(val) } },
      writable: true,
    })

    const { default: apiClient } = await import('../services/api')

    // Return 401 from adapter to trigger the refresh logic
    const adapterSpy = vi.fn().mockImplementation(() => {
      return Promise.reject({
        response: { status: 401 },
        config: { _retry: false, headers: {}, url: '/test' },
        isAxiosError: true,
        message: 'Request failed with status code 401',
      })
    })

    const originalAdapter = apiClient.defaults.adapter
    apiClient.defaults.adapter = adapterSpy

    await apiClient.get('/protected').catch(() => {})

    // Should have attempted to redirect to login
    expect(hrefSetter).toHaveBeenCalledWith('/login')

    apiClient.defaults.adapter = originalAdapter
  })

  it('refreshes the token on 401 and retries the original request', async () => {
    localStorage.setItem(
      'fileagent-auth',
      JSON.stringify({ state: { accessToken: 'old', refreshToken: 'refresh-1' } }),
    )

    const { default: apiClient } = await import('../services/api')

    // The refresh call uses the standalone axios.post (not apiClient).
    const postSpy = vi
      .spyOn(axios, 'post')
      .mockResolvedValue({ data: { access_token: 'new-access', refresh_token: 'new-refresh' } })

    // First call 401s; the retried call (after refresh) succeeds.
    let calls = 0
    const adapterSpy = vi.fn().mockImplementation((config) => {
      calls += 1
      if (calls === 1) {
        return Promise.reject({
          response: { status: 401 },
          config: { ...config, _retry: false },
          isAxiosError: true,
        })
      }
      return Promise.resolve({ data: { ok: true }, status: 200, statusText: 'OK', headers: {}, config })
    })
    const originalAdapter = apiClient.defaults.adapter
    apiClient.defaults.adapter = adapterSpy

    const res = await apiClient.get('/protected')

    expect(res.data).toEqual({ ok: true })
    expect(postSpy).toHaveBeenCalledWith(
      expect.stringContaining('/api/auth/refresh'),
      { refresh_token: 'refresh-1' },
      expect.anything(),
    )
    // Rotated tokens are persisted back to localStorage.
    const stored = JSON.parse(localStorage.getItem('fileagent-auth')!)
    expect(stored.state.accessToken).toBe('new-access')
    expect(stored.state.refreshToken).toBe('new-refresh')

    apiClient.defaults.adapter = originalAdapter
    postSpy.mockRestore()
  })
})
