import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// Mock apiClient before importing the service under test.
const mockGet = vi.fn()
const mockPost = vi.fn()
const mockPatch = vi.fn()
const mockDelete = vi.fn()

vi.mock('../services/api', () => ({
  default: {
    get: (...args: unknown[]) => mockGet(...args),
    post: (...args: unknown[]) => mockPost(...args),
    patch: (...args: unknown[]) => mockPatch(...args),
    delete: (...args: unknown[]) => mockDelete(...args),
  },
}))

describe('services/tags', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('listTagKeys unwraps items from the list envelope', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ key: 'site' }], total: 1 } })
    const { listTagKeys } = await import('../services/tags')
    const result = await listTagKeys()
    expect(mockGet).toHaveBeenCalledWith('/api/v1/tag-keys')
    expect(result).toHaveLength(1)
    expect(result[0].key).toBe('site')
  })

  it('createTagKey POSTs the body', async () => {
    mockPost.mockResolvedValue({ data: { id: '1', key: 'site' } })
    const { createTagKey } = await import('../services/tags')
    const body = { key: 'site', label: '站点', value_controlled: true }
    const result = await createTagKey(body)
    expect(mockPost).toHaveBeenCalledWith('/api/v1/tag-keys', body)
    expect(result.key).toBe('site')
  })

  it('updateTagKey PATCHes the encoded key path', async () => {
    mockPatch.mockResolvedValue({ data: { key: 'a/b', label: 'New' } })
    const { updateTagKey } = await import('../services/tags')
    // A key with a reserved char proves encodeURIComponent is applied.
    await updateTagKey('a/b', { label: 'New' })
    expect(mockPatch).toHaveBeenCalledWith('/api/v1/tag-keys/a%2Fb', { label: 'New' })
  })

  it('deleteTagKey DELETEs the encoded key path', async () => {
    mockDelete.mockResolvedValue({ data: {} })
    const { deleteTagKey } = await import('../services/tags')
    await deleteTagKey('a/b')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/tag-keys/a%2Fb')
  })

  it('listTagValues unwraps items for a key (encoded)', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ id: 'v1', value: 'tokyo' }], total: 1 } })
    const { listTagValues } = await import('../services/tags')
    const result = await listTagValues('a/b')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/tag-keys/a%2Fb/values')
    expect(result[0].value).toBe('tokyo')
  })

  it('createTagValue POSTs the value', async () => {
    mockPost.mockResolvedValue({ data: { id: 'v1', value: 'tokyo' } })
    const { createTagValue } = await import('../services/tags')
    await createTagValue('site', 'tokyo')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/tag-keys/site/values', { value: 'tokyo' })
  })

  it('deleteTagValue DELETEs the value path by id', async () => {
    mockDelete.mockResolvedValue({ data: {} })
    const { deleteTagValue } = await import('../services/tags')
    await deleteTagValue('site', 'v1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/tag-keys/site/values/v1')
  })
})
