import apiClient from './api'

/** Upload log entity */
export interface UploadLog {
  id: string
  agent_id: string
  file_id: string | null
  filename: string
  size: number
  // Upload logs are terminal: the CP creates them only after an upload result,
  // so the only values are completed/failed (upper-cased at the REST boundary).
  status: 'COMPLETED' | 'FAILED'
  error_message: string | null
  // Retry-trail fields (4e / D-027). The current controlplane always sends
  // retry_count / bytes_transferred / started_at (started_at is NOT NULL);
  // finished_at is nullable. All are optional here only for resilience against
  // an older controlplane that predates these projected fields — the UI degrades
  // gracefully to "—" when a key is missing.
  /** Number of retries the agent made. */
  retry_count?: number
  /** Bytes transferred (partial on failure). */
  bytes_transferred?: number
  /** When the upload attempt started. */
  started_at?: string
  /** When the upload finished (null until terminal). */
  finished_at?: string
  uploaded_at: string
}

/** Query parameters for listing upload logs */
export interface ListUploadLogsParams {
  agent_id?: string
  status?: string
  cursor?: string
  limit?: number
  since?: string
  until?: string
}

/** Paginated list response */
export interface PaginatedResponse<T> {
  items: T[]
  total: number
  next_cursor: string | null
  has_more: boolean
}

/**
 * List upload logs with optional filtering (cursor-based pagination).
 */
export async function listUploadLogs(
  params?: ListUploadLogsParams
): Promise<PaginatedResponse<UploadLog>> {
  const response = await apiClient.get<PaginatedResponse<UploadLog>>(
    '/api/v1/upload-logs',
    { params }
  )
  return response.data
}

/**
 * Get a single upload log by ID.
 */
export async function getUploadLog(id: string): Promise<UploadLog> {
  const response = await apiClient.get<UploadLog>(`/api/v1/upload-logs/${id}`)
  return response.data
}
