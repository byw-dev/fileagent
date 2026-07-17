import { describe, it, expect, vi, beforeEach } from 'vitest'

// Mock the shared axios client before importing any service. Each service is
// exercised for real (only the HTTP layer is stubbed) so its code counts toward
// coverage — the page tests mock the services themselves and don't (WR-10 §1).
const mockGet = vi.fn()
const mockPost = vi.fn()
const mockPut = vi.fn()
const mockPatch = vi.fn()
const mockDelete = vi.fn()

vi.mock('../services/api', () => ({
  default: {
    get: (...a: unknown[]) => mockGet(...a),
    post: (...a: unknown[]) => mockPost(...a),
    put: (...a: unknown[]) => mockPut(...a),
    patch: (...a: unknown[]) => mockPatch(...a),
    delete: (...a: unknown[]) => mockDelete(...a),
  },
}))

beforeEach(() => vi.clearAllMocks())

// ── agents (top-level lifecycle functions) ────────────────────────────────────
describe('services/agents – lifecycle', () => {
  it('listAgents GETs /agents with params', async () => {
    mockGet.mockResolvedValue({ data: { items: [], total: 0, next_cursor: null, has_more: false } })
    const { listAgents } = await import('../services/agents')
    const res = await listAgents({ status: 'RUNNING', limit: 50 })
    expect(mockGet).toHaveBeenCalledWith('/api/v1/agents', { params: { status: 'RUNNING', limit: 50 } })
    expect(res.total).toBe(0)
  })

  it('getAgent GETs /agents/:id', async () => {
    mockGet.mockResolvedValue({ data: { id: 'a1' } })
    const { getAgent } = await import('../services/agents')
    expect((await getAgent('a1')).id).toBe('a1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/agents/a1')
  })

  it('approveAgent POSTs /agents/:id/approve', async () => {
    mockPost.mockResolvedValue({ data: { id: 'a1', status: 'APPROVED' } })
    const { approveAgent } = await import('../services/agents')
    expect((await approveAgent('a1')).status).toBe('APPROVED')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/agents/a1/approve')
  })

  it('revokeAgent POSTs /agents/:id/revoke', async () => {
    mockPost.mockResolvedValue({ data: { id: 'a1', status: 'REVOKED' } })
    const { revokeAgent } = await import('../services/agents')
    expect((await revokeAgent('a1')).status).toBe('REVOKED')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/agents/a1/revoke')
  })

  it('deleteAgent DELETEs /agents/:id', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteAgent } = await import('../services/agents')
    await deleteAgent('a1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/agents/a1')
  })

  it('renameAgent PATCHes /agents/:id with name', async () => {
    mockPatch.mockResolvedValue({ data: { id: 'a1', name: 'edge-1' } })
    const { renameAgent } = await import('../services/agents')
    expect((await renameAgent('a1', 'edge-1')).name).toBe('edge-1')
    expect(mockPatch).toHaveBeenCalledWith('/api/v1/agents/a1', { name: 'edge-1' })
  })

  it('testRule POSTs /agents/:id/test-rule', async () => {
    mockPost.mockResolvedValue({ data: { files: [] } })
    const { testRule } = await import('../services/agents')
    const params = { base_path: '/d', path_pattern: '*.csv', dest_path_template: '/{filename}' }
    await testRule('a1', params)
    expect(mockPost).toHaveBeenCalledWith('/api/v1/agents/a1/test-rule', params)
  })
})

// ── files (gaps) ──────────────────────────────────────────────────────────────
describe('services/files – gaps', () => {
  it('getFile GETs /files/:id', async () => {
    mockGet.mockResolvedValue({ data: { id: 'f1' } })
    const { getFile } = await import('../services/files')
    expect((await getFile('f1')).id).toBe('f1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/files/f1')
  })

  it('deleteFile DELETEs /files/:id', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteFile } = await import('../services/files')
    await deleteFile('f1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/files/f1')
  })
})

// ── buckets ───────────────────────────────────────────────────────────────────
describe('services/buckets', () => {
  it('listBuckets GETs /buckets and unwraps items', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ id: 'b1' }], total: 1 } })
    const { listBuckets } = await import('../services/buckets')
    expect(await listBuckets()).toHaveLength(1)
    expect(mockGet).toHaveBeenCalledWith('/api/v1/buckets')
  })

  it('createBucket POSTs /buckets', async () => {
    mockPost.mockResolvedValue({ data: { id: 'b1', name: 'logs' } })
    const { createBucket } = await import('../services/buckets')
    expect((await createBucket({ name: 'logs' })).name).toBe('logs')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/buckets', { name: 'logs' })
  })
})

// ── event-rules ───────────────────────────────────────────────────────────────
describe('services/events', () => {
  it('listEventRules GETs /event-rules and unwraps items', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ id: 'r1' }], total: 1 } })
    const { listEventRules } = await import('../services/events')
    expect(await listEventRules()).toHaveLength(1)
    expect(mockGet).toHaveBeenCalledWith('/api/v1/event-rules')
  })

  it('getEventRule GETs /event-rules/:id', async () => {
    mockGet.mockResolvedValue({ data: { id: 'r1' } })
    const { getEventRule } = await import('../services/events')
    expect((await getEventRule('r1')).id).toBe('r1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/event-rules/r1')
  })

  it('createEventRule POSTs /event-rules', async () => {
    mockPost.mockResolvedValue({ data: { id: 'r1' } })
    const { createEventRule } = await import('../services/events')
    const body = { name: 'n', event_type: 'file_uploaded', action_type: 'webhook', action_config: {}, enabled: true }
    await createEventRule(body)
    expect(mockPost).toHaveBeenCalledWith('/api/v1/event-rules', body)
  })

  it('updateEventRule PUTs /event-rules/:id', async () => {
    mockPut.mockResolvedValue({ data: { id: 'r1' } })
    const { updateEventRule } = await import('../services/events')
    const body = { name: 'n', event_type: 'file_uploaded', action_type: 'webhook', action_config: {}, enabled: false }
    await updateEventRule('r1', body)
    expect(mockPut).toHaveBeenCalledWith('/api/v1/event-rules/r1', body)
  })

  it('deleteEventRule DELETEs /event-rules/:id', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteEventRule } = await import('../services/events')
    await deleteEventRule('r1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/event-rules/r1')
  })

  it('listRuleDeliveries GETs /event-rules/:id/deliveries with params', async () => {
    mockGet.mockResolvedValue({ data: { items: [], total: 0, next_cursor: null, has_more: false } })
    const { listRuleDeliveries } = await import('../services/events')
    await listRuleDeliveries('r1', { limit: 50 })
    expect(mockGet).toHaveBeenCalledWith('/api/v1/event-rules/r1/deliveries', { params: { limit: 50 } })
  })
})

// ── file-types ────────────────────────────────────────────────────────────────
describe('services/file-types', () => {
  it('listFileTypes GETs /file-types and unwraps items', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ id: 't1' }], total: 1 } })
    const { listFileTypes } = await import('../services/file-types')
    expect(await listFileTypes()).toHaveLength(1)
    expect(mockGet).toHaveBeenCalledWith('/api/v1/file-types')
  })

  it('getFileType GETs /file-types/:id', async () => {
    mockGet.mockResolvedValue({ data: { id: 't1' } })
    const { getFileType } = await import('../services/file-types')
    expect((await getFileType('t1')).id).toBe('t1')
    expect(mockGet).toHaveBeenCalledWith('/api/v1/file-types/t1')
  })

  it('createFileType POSTs /file-types', async () => {
    mockPost.mockResolvedValue({ data: { id: 't1', name: 'pressure' } })
    const { createFileType } = await import('../services/file-types')
    expect((await createFileType({ name: 'pressure' })).name).toBe('pressure')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/file-types', { name: 'pressure' })
  })

  it('updateFileType PUTs /file-types/:id', async () => {
    mockPut.mockResolvedValue({ data: { id: 't1', name: 'p2' } })
    const { updateFileType } = await import('../services/file-types')
    await updateFileType('t1', { name: 'p2' })
    expect(mockPut).toHaveBeenCalledWith('/api/v1/file-types/t1', { name: 'p2' })
  })

  it('deleteFileType DELETEs /file-types/:id', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteFileType } = await import('../services/file-types')
    await deleteFileType('t1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/file-types/t1')
  })
})

// ── users ─────────────────────────────────────────────────────────────────────
describe('services/users', () => {
  it('listUsers GETs /users and unwraps items', async () => {
    mockGet.mockResolvedValue({ data: { items: [{ id: 'u1' }], total: 1 } })
    const { listUsers } = await import('../services/users')
    expect(await listUsers()).toHaveLength(1)
    expect(mockGet).toHaveBeenCalledWith('/api/v1/users')
  })

  it('createUser POSTs /users', async () => {
    mockPost.mockResolvedValue({ data: { id: 'u1', username: 'bob' } })
    const { createUser } = await import('../services/users')
    const body = { username: 'bob', password: 'secret12', role: 'org_viewer' }
    expect((await createUser(body)).username).toBe('bob')
    expect(mockPost).toHaveBeenCalledWith('/api/v1/users', body)
  })

  it('updateUser PUTs /users/:id', async () => {
    mockPut.mockResolvedValue({ data: { id: 'u1' } })
    const { updateUser } = await import('../services/users')
    await updateUser('u1', { role: 'org_admin' })
    expect(mockPut).toHaveBeenCalledWith('/api/v1/users/u1', { role: 'org_admin' })
  })

  it('setUserActive PUTs /users/:id/active with is_active', async () => {
    mockPut.mockResolvedValue({ data: { id: 'u1', is_active: false } })
    const { setUserActive } = await import('../services/users')
    expect((await setUserActive('u1', false)).is_active).toBe(false)
    expect(mockPut).toHaveBeenCalledWith('/api/v1/users/u1/active', { is_active: false })
  })

  it('deleteUser DELETEs /users/:id', async () => {
    mockDelete.mockResolvedValue({})
    const { deleteUser } = await import('../services/users')
    await deleteUser('u1')
    expect(mockDelete).toHaveBeenCalledWith('/api/v1/users/u1')
  })

  it('updateUserPassword PUTs /users/:id/password', async () => {
    mockPut.mockResolvedValue({})
    const { updateUserPassword } = await import('../services/users')
    await updateUserPassword('u1', 'newpass12')
    expect(mockPut).toHaveBeenCalledWith('/api/v1/users/u1/password', { password: 'newpass12' })
  })
})
