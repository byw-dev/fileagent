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
