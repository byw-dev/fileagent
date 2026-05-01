import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// We mock apiClient before importing services
const mockGet = vi.fn()
const mockPost = vi.fn()
const mockPut = vi.fn()
const mockDelete = vi.fn()

vi.mock('../services/api', () => ({
  default: {
    get: (...args: unknown[]) => mockGet(...args),
    post: (...args: unknown[]) => mockPost(...args),
    put: (...args: unknown[]) => mockPut(...args),
    delete: (...args: unknown[]) => mockDelete(...args),
  },
}))

describe('services/upload-logs', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('listUploadLogs calls GET /api/v1/upload-logs', async () => {
    mockGet.mockResolvedValue({
      data: { items: [], total: 0, next_cursor: null },
    })
    const { listUploadLogs } = await import('../services/upload-logs')
    const result = await listUploadLogs()
    expect(mockGet).toHaveBeenCalledWith('/api/v1/upload-logs', { params: undefined })
    expect(result.total).toBe(0)
  })

  it('listUploadLogs passes params', async () => {
    mockGet.mockResolvedValue({
      data: { items: [], total: 5, next_cursor: null },
    })
    const { listUploadLogs } = await import('../services/upload-logs')
    await listUploadLogs({ agent_id: 'agent-1', limit: 20 })
    expect(mockGet).toHaveBeenCalledWith('/api/v1/upload-logs', {
      params: { agent_id: 'agent-1', limit: 20 },
    })
  })

  it('getUploadLog calls GET /api/v1/upload-logs/:id', async () => {
    const log = {
      id: 'log-1',
      filename: 'test.csv',
      size: 1024,
      status: 'SUCCESS',
      uploaded_at: '2025-01-01T00:00:00Z',
    }
    mockGet.mockResolvedValue({ data: log })
    const { getUploadLog } = await import('../services/upload-logs')
    const result = await getUploadLog('log-1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/upload-logs/log-1')
    expect(result.id).toBe('log-1')
  })
})

describe('services/agents – extended functions', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('listRules calls GET /api/v1/agents/:id/rules', async () => {
    mockGet.mockResolvedValue({
      data: { items: [], total: 0, next_cursor: null },
    })
    const { listRules } = await import('../services/agents')
    await listRules('agent-1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/agents/agent-1/rules', {
      params: undefined,
    })
  })

  it('createRule calls POST /api/v1/agents/:id/rules', async () => {
    const rule = {
      id: 'rule-1',
      agent_id: 'agent-1',
      name: 'Test Rule',
      mode: 'WATCH' as const,
      source_path: '/data',
      file_pattern: '*.csv',
      cron_expr: null,
      run_once_on_start: false,
      dest_bucket_id: 'bucket-1',
      dest_path_template: '/{year}/{filename}',
      is_active: true,
      created_at: '',
      updated_at: '',
    }
    mockPost.mockResolvedValue({ data: rule })
    const { createRule } = await import('../services/agents')
    const payload = {
      name: 'Test Rule',
      mode: 'WATCH' as const,
      source_path: '/data',
      file_pattern: '*.csv',
      cron_expr: null,
      run_once_on_start: false,
      dest_bucket_id: 'bucket-1',
      dest_path_template: '/{year}/{filename}',
    }
    const result = await createRule('agent-1', payload)
    expect(mockPost).toHaveBeenCalledWith('/api/v1/agents/agent-1/rules', payload)
    expect(result.id).toBe('rule-1')
  })

  it('deleteRule calls DELETE /api/v1/agents/:id/rules/:rid', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteRule } = await import('../services/agents')
    await deleteRule('agent-1', 'rule-1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/agents/agent-1/rules/rule-1')
  })

  it('listDir calls POST /api/v1/agents/:id/list-dir', async () => {
    mockPost.mockResolvedValue({ data: [{ name: 'data', path: '/data', is_dir: true, size: null, modified_at: null }] })
    const { listDir } = await import('../services/agents')
    const result = await listDir('agent-1', '/')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/agents/agent-1/list-dir', { path: '/' })
    expect(result).toHaveLength(1)
  })

  it('listAgentUploadLogs calls GET /api/v1/agents/:id/upload-logs', async () => {
    mockGet.mockResolvedValue({
      data: { items: [], total: 0, next_cursor: null },
    })
    const { listAgentUploadLogs } = await import('../services/agents')
    await listAgentUploadLogs('agent-1', { limit: 50 })
    expect(mockGet).toHaveBeenCalledWith('/api/v1/agents/agent-1/upload-logs', {
      params: { limit: 50 },
    })
  })

  it('updateRule calls PUT /api/v1/agents/:id/rules/:rid', async () => {
    mockPut.mockResolvedValue({ data: { id: 'rule-1', name: 'Updated' } })
    const { updateRule } = await import('../services/agents')
    await updateRule('agent-1', 'rule-1', { name: 'Updated' })
    expect(mockPut).toHaveBeenCalledWith('/api/v1/agents/agent-1/rules/rule-1', {
      name: 'Updated',
    })
  })
})

describe('services/files – extended coverage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('listFiles calls GET /api/v1/files', async () => {
    mockGet.mockResolvedValue({
      data: { items: [], total: 0, next_cursor: null },
    })
    const { listFiles } = await import('../services/files')
    const result = await listFiles()
    expect(mockGet).toHaveBeenCalledWith('/api/v1/files', { params: undefined })
    expect(result.total).toBe(0)
  })

  it('getFileDownloadUrl calls GET /api/v1/files/:id/download', async () => {
    mockGet.mockResolvedValue({ data: { url: 'https://example.com/file' } })
    const { getFileDownloadUrl } = await import('../services/files')
    const result = await getFileDownloadUrl('file-1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/files/file-1/download')
    expect(result.url).toBe('https://example.com/file')
  })
})

describe('utils/pathTemplate – coverage for renderPathPreview', () => {
  it('renderPathPreview substitutes all known variables', async () => {
    const { renderPathPreview } = await import('../utils/pathTemplate')
    const result = renderPathPreview('/{year}/{month}/{day}/{hour}/{agent_id}/{agent_name}/{file_type}/{filename}/{ext}')
    expect(result).not.toContain('{year}')
    expect(result).not.toContain('{month}')
    expect(result).not.toContain('{filename}')
  })

  it('validatePathTemplate returns null for valid template', async () => {
    const { validatePathTemplate } = await import('../utils/pathTemplate')
    expect(validatePathTemplate('/{year}/{month}/{filename}')).toBeNull()
  })

  it('validatePathTemplate rejects empty string', async () => {
    const { validatePathTemplate } = await import('../utils/pathTemplate')
    expect(validatePathTemplate('')).toBeTruthy()
  })

  it('validatePathTemplate rejects template not starting with /', async () => {
    const { validatePathTemplate } = await import('../utils/pathTemplate')
    expect(validatePathTemplate('year/month')).toBeTruthy()
  })

  it('validatePathTemplate rejects unknown variables', async () => {
    const { validatePathTemplate } = await import('../utils/pathTemplate')
    expect(validatePathTemplate('/{year}/{unknown_var}')).toBeTruthy()
  })
})
