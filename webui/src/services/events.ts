import apiClient from './api'

/** Event rule entity matching the backend /api/v1/event-rules response */
export interface EventRule {
  id: string
  org_id: string
  name: string
  event_type: string
  filter: Record<string, unknown>
  action_type: string
  action_config: Record<string, unknown>
  enabled: boolean
  created_at: string
}

/** Event delivery record */
export interface EventDelivery {
  id: string
  event_rule_id: string
  status: string
  attempt_count: number
  created_at: string
}

/** Paginated deliveries response */
export interface DeliveriesResponse {
  items: EventDelivery[]
  total: number
  next_cursor: string | null
}

/**
 * List all event rules in the organisation.
 */
export async function listEventRules(): Promise<EventRule[]> {
  const response = await apiClient.get<{ items: EventRule[]; total: number }>('/api/v1/event-rules')
  return response.data.items
}

/**
 * Get a single event rule by ID.
 */
export async function getEventRule(id: string): Promise<EventRule> {
  const response = await apiClient.get<EventRule>(`/api/v1/event-rules/${id}`)
  return response.data
}

/**
 * Create a new event rule.
 */
export async function createEventRule(data: {
  name: string
  event_type: string
  filter?: Record<string, unknown>
  action_type: string
  action_config: Record<string, unknown>
  enabled: boolean
}): Promise<EventRule> {
  const response = await apiClient.post<EventRule>('/api/v1/event-rules', data)
  return response.data
}

/**
 * Update an existing event rule.
 */
export async function updateEventRule(
  id: string,
  data: {
    name: string
    event_type: string
    filter?: Record<string, unknown>
    action_type: string
    action_config: Record<string, unknown>
    enabled: boolean
  }
): Promise<EventRule> {
  const response = await apiClient.put<EventRule>(`/api/v1/event-rules/${id}`, data)
  return response.data
}

/**
 * Delete an event rule.
 */
export async function deleteEventRule(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/event-rules/${id}`)
}

/**
 * List delivery records for a specific event rule.
 */
export async function listRuleDeliveries(
  ruleId: string,
  params?: { cursor?: string; limit?: number }
): Promise<DeliveriesResponse> {
  const response = await apiClient.get<DeliveriesResponse>(
    `/api/v1/event-rules/${ruleId}/deliveries`,
    { params }
  )
  return response.data
}
