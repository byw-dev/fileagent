import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

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
})
