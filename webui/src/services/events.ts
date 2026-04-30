import apiClient from './api'

/** Event rule entity */
export interface EventRule {
  id: string
  name: string
  description: string
  event_type: string
  filter_conditions: Record<string, unknown>
  webhook_url: string
  webhook_secret: string | null
  is_active: boolean
  created_at: string
  updated_at: string
}

/** Event delivery record */
export interface EventDelivery {
  id: string
  event_rule_id: string
  event_type: string
  payload: Record<string, unknown>
  status: 'PENDING' | 'SUCCESS' | 'FAILED'
  attempt_count: number
  last_attempted_at: string | null
  next_retry_at: string | null
  response_code: number | null
  response_body: string | null
  created_at: string
}

/** Query parameters for listing event rules */
export interface ListEventRulesParams {
  cursor?: string
  limit?: number
}

/** Query parameters for listing deliveries */
export interface ListDeliveriesParams {
  event_rule_id?: string
  status?: string
  cursor?: string
  limit?: number
}

/** Paginated list response */
export interface PaginatedResponse<T> {
  items: T[]
  total: number
  next_cursor: string | null
}

/**
 * List event rules.
 */
export async function listEventRules(
  params?: ListEventRulesParams
): Promise<PaginatedResponse<EventRule>> {
  const response = await apiClient.get<PaginatedResponse<EventRule>>(
    '/api/v1/events',
    { params }
  )
  return response.data
}

/**
 * Get a single event rule by ID.
 */
export async function getEventRule(id: string): Promise<EventRule> {
  const response = await apiClient.get<EventRule>(`/api/v1/events/${id}`)
  return response.data
}

/**
 * Create a new event rule.
 */
export async function createEventRule(
  data: Omit<EventRule, 'id' | 'created_at' | 'updated_at'>
): Promise<EventRule> {
  const response = await apiClient.post<EventRule>('/api/v1/events', data)
  return response.data
}

/**
 * Update an existing event rule.
 */
export async function updateEventRule(
  id: string,
  data: Partial<EventRule>
): Promise<EventRule> {
  const response = await apiClient.put<EventRule>(`/api/v1/events/${id}`, data)
  return response.data
}

/**
 * Delete an event rule.
 */
export async function deleteEventRule(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/events/${id}`)
}

/**
 * List event deliveries.
 */
export async function listEventDeliveries(
  params?: ListDeliveriesParams
): Promise<PaginatedResponse<EventDelivery>> {
  const response = await apiClient.get<PaginatedResponse<EventDelivery>>(
    '/api/v1/event-deliveries',
    { params }
  )
  return response.data
}
