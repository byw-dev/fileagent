import apiClient from './api'

/** Agent status values matching the backend enum */
export type AgentStatus =
  | 'PENDING'
  | 'APPROVED'
  | 'RUNNING'
  | 'OFFLINE'
  | 'REVOKED'

/** Agent entity */
export interface Agent {
  id: string
  name: string
  hostname: string
  ip_address: string
  status: AgentStatus
  version: string
  os: string
  last_heartbeat_at: string | null
  registered_at: string
  org_id: string
}

/** Paginated list response */
export interface PaginatedResponse<T> {
  items: T[]
  total: number
  next_cursor: string | null
}

/** Query parameters for listing agents */
export interface ListAgentsParams {
  status?: AgentStatus
  cursor?: string
  limit?: number
}

/**
 * List all agents with optional filtering.
 */
export async function listAgents(
  params?: ListAgentsParams
): Promise<PaginatedResponse<Agent>> {
  const response = await apiClient.get<PaginatedResponse<Agent>>(
    '/api/v1/agents',
    { params }
  )
  return response.data
}

/**
 * Get a single agent by ID.
 */
export async function getAgent(id: string): Promise<Agent> {
  const response = await apiClient.get<Agent>(`/api/v1/agents/${id}`)
  return response.data
}

/**
 * Approve a pending agent.
 */
export async function approveAgent(id: string): Promise<Agent> {
  const response = await apiClient.post<Agent>(
    `/api/v1/agents/${id}/approve`
  )
  return response.data
}

/**
 * Revoke an approved agent.
 */
export async function revokeAgent(id: string): Promise<Agent> {
  const response = await apiClient.post<Agent>(`/api/v1/agents/${id}/revoke`)
  return response.data
}

/**
 * Delete an agent.
 */
export async function deleteAgent(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/agents/${id}`)
}

/** Collection mode for a rule */
export type CollectionMode = 'WATCH' | 'SCHEDULED'

/** Collection rule entity */
export interface CollectionRule {
  id: string
  agent_id: string
  name: string
  mode: CollectionMode
  source_path: string
  file_pattern: string
  cron_expr: string | null
  run_once_on_start: boolean
  dest_bucket_id: string
  dest_path_template: string
  is_active: boolean
  created_at: string
  updated_at: string
}

/** Create/update payload for a collection rule */
export interface CollectionRulePayload {
  name: string
  mode: CollectionMode
  source_path: string
  file_pattern: string
  cron_expr: string | null
  run_once_on_start: boolean
  dest_bucket_id: string
  dest_path_template: string
}

/** Directory listing item */
export interface DirEntry {
  name: string
  path: string
  is_dir: boolean
  size: number | null
  modified_at: string | null
}

/**
 * List all collection rules for an agent.
 */
export async function listRules(
  agentId: string,
  params?: { cursor?: string; limit?: number }
): Promise<PaginatedResponse<CollectionRule>> {
  const response = await apiClient.get<PaginatedResponse<CollectionRule>>(
    `/api/v1/agents/${agentId}/rules`,
    { params }
  )
  return response.data
}

/**
 * Create a new collection rule for an agent.
 */
export async function createRule(
  agentId: string,
  data: CollectionRulePayload
): Promise<CollectionRule> {
  const response = await apiClient.post<CollectionRule>(
    `/api/v1/agents/${agentId}/rules`,
    data
  )
  return response.data
}

/**
 * Update an existing collection rule.
 */
export async function updateRule(
  agentId: string,
  ruleId: string,
  data: Partial<CollectionRulePayload>
): Promise<CollectionRule> {
  const response = await apiClient.put<CollectionRule>(
    `/api/v1/agents/${agentId}/rules/${ruleId}`,
    data
  )
  return response.data
}

/**
 * Delete a collection rule.
 */
export async function deleteRule(agentId: string, ruleId: string): Promise<void> {
  await apiClient.delete(`/api/v1/agents/${agentId}/rules/${ruleId}`)
}

/**
 * List directory contents on the remote agent.
 */
export async function listDir(agentId: string, path: string): Promise<DirEntry[]> {
  const response = await apiClient.post<DirEntry[]>(
    `/api/v1/agents/${agentId}/list-dir`,
    { path }
  )
  return response.data
}

/**
 * List upload logs for a specific agent.
 */
export async function listAgentUploadLogs(
  agentId: string,
  params?: { cursor?: string; limit?: number; status?: string }
): Promise<PaginatedResponse<{ id: string; filename: string; size: number; status: string; uploaded_at: string }>> {
  const response = await apiClient.get(
    `/api/v1/agents/${agentId}/upload-logs`,
    { params }
  )
  return response.data
}
