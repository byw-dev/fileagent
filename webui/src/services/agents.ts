import apiClient from './api'
import type { UploadLog, PaginatedResponse as UploadLogPaginatedResponse } from './upload-logs'

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
  is_online: boolean
  os_type: string
  os_version: string
  agent_version: string
  last_seen_at: string | null
  created_at: string
  org_id: string
}

/** Paginated list response */
export interface PaginatedResponse<T> {
  items: T[]
  total: number
  next_cursor: string | null
  has_more: boolean
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

/**
 * Rename an agent (set a custom display name). super_admin only.
 */
export async function renameAgent(id: string, name: string): Promise<Agent> {
  const response = await apiClient.patch<Agent>(`/api/v1/agents/${id}`, { name })
  return response.data
}

/** Collection mode for a rule */
export type CollectionMode = 'WATCH' | 'SCHEDULED'

/** Collection rule entity */
/** Rule metadata declaration (metadata 6c) stored in collection_rules.metadata.
 * file_type = declared coarse type (overrides glob); static_tags = fixed tags on
 * every collected file; path_tag_map = tag key → path-template variable. */
export interface RuleMetadata {
  file_type?: string
  static_tags?: Record<string, string>
  path_tag_map?: Record<string, string>
}

export interface CollectionRule {
  id: string
  agent_id: string
  name: string
  mode: CollectionMode
  base_path: string
  path_pattern: string
  cron_expr: string | null
  run_once_on_start: boolean
  bucket_id: string
  dest_path_template: string
  recursive: boolean
  append_mode: string
  enabled: boolean
  metadata?: RuleMetadata
  created_at: string
}

/** Create/update payload for a collection rule */
export interface CollectionRulePayload {
  name: string
  mode: CollectionMode
  base_path: string
  path_pattern: string
  cron_expr: string | null
  run_once_on_start: boolean
  bucket_id: string
  dest_path_template: string
  recursive: boolean
  append_mode?: string
  enabled?: boolean
  metadata?: RuleMetadata
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

/** Response shape for POST /api/v1/agents/:id/list-dir */
export interface ListDirResponse {
  path: string
  entries: DirEntry[]
}

/**
 * List directory contents on the remote agent.
 * Blocks until the agent responds (up to 30 s server-side timeout).
 */
export async function listDir(agentId: string, path: string): Promise<ListDirResponse> {
  const response = await apiClient.post<ListDirResponse>(
    `/api/v1/agents/${agentId}/list-dir`,
    { path }
  )
  return response.data
}

/** Parameters for POST /api/v1/agents/:id/test-rule */
export interface TestRuleParams {
  base_path: string
  path_pattern: string
  dest_path_template: string
  recursive?: boolean
  dry_run_limit?: number
}

/** A single matched file in the dry-run result */
export interface TestRuleFileResult {
  local_path: string
  upload_path: string
  parsed_fields: Record<string, string>
  compose_error?: string
}

/** Response from POST /api/v1/agents/:id/test-rule */
export interface TestRuleResult {
  files: TestRuleFileResult[]
}

/**
 * Dry-run a collection rule against a live agent.
 * The agent walks base_path, matches files, and returns path mappings.
 * Blocks until the agent responds (up to 30 s server-side timeout).
 */
export async function testRule(agentId: string, params: TestRuleParams): Promise<TestRuleResult> {
  const response = await apiClient.post<TestRuleResult>(
    `/api/v1/agents/${agentId}/test-rule`,
    params
  )
  return response.data
}

/**
 * List upload logs for a specific agent.
 */
export async function listAgentUploadLogs(
  agentId: string,
  params?: { cursor?: string; limit?: number; status?: string }
): Promise<UploadLogPaginatedResponse<UploadLog>> {
  const response = await apiClient.get<UploadLogPaginatedResponse<UploadLog>>(
    `/api/v1/agents/${agentId}/upload-logs`,
    { params }
  )
  return response.data
}
