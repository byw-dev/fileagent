import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

const mockGet = vi.fn()
const mockPost = vi.fn()

vi.mock('../services/api', () => ({
  default: {
    get: (...args: unknown[]) => mockGet(...args),
    post: (...args: unknown[]) => mockPost(...args),
  },
}))

describe('services/pending-tags', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('listPendingTagValues unwraps items', async () => {
    mockGet.mockResolvedValue({
      data: { items: [{ id: '1', key: 'site', extracted_value: 'tyo' }], total: 1 },
    })
    const { listPendingTagValues } = await import('../services/pending-tags')
    const result = await listPendingTagValues()
    expect(mockGet).toHaveBeenCalledWith('/api/v1/pending-tag-values')
    expect(result[0].extracted_value).toBe('tyo')
  })

  it('approvePendingTagValue POSTs the approve path', async () => {
    mockPost.mockResolvedValue({ data: {} })
    const { approvePendingTagValue } = await import('../services/pending-tags')
    await approvePendingTagValue('abc')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/pending-tag-values/abc/approve')
  })

  it('mergePendingTagValue POSTs the into target and returns the job', async () => {
    mockPost.mockResolvedValue({ data: { job_id: 'j1', status: 'pending', into: 'Tokyo' } })
    const { mergePendingTagValue } = await import('../services/pending-tags')
    const res = await mergePendingTagValue('abc', 'Tokyo')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/pending-tag-values/abc/merge', { into: 'Tokyo' })
    expect(res.job_id).toBe('j1')
  })

  it('mergePendingTagValue sends empty body when into is omitted', async () => {
    mockPost.mockResolvedValue({ data: { job_id: 'j1', status: 'pending', into: 'Tokyo' } })
    const { mergePendingTagValue } = await import('../services/pending-tags')
    await mergePendingTagValue('abc')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/pending-tag-values/abc/merge', {})
  })

  it('rejectPendingTagValue POSTs the reject path', async () => {
    mockPost.mockResolvedValue({ data: {} })
    const { rejectPendingTagValue } = await import('../services/pending-tags')
    await rejectPendingTagValue('abc')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/pending-tag-values/abc/reject')
  })
})
