import apiClient from './api'

/** Upload log entity */
export interface UploadLog {
  id: string
  agent_id: string
  /** Linked file entry; omitempty on the backend → absent (undefined) when none. */
  file_id?: string
  filename: string
  size: number
  // Upload logs are terminal: the CP creates them only after an upload result,
  // so the only values are completed/failed (upper-cased at the REST boundary).
  status: 'COMPLETED' | 'FAILED'
  /** Error text on failure; omitempty on the backend → absent when none. */
  error_message?: string
  // Retry-trail fields (4e / D-027). Frontend and controlplane ship together, so
  // these mirror the live JSON exactly: retry_count / bytes_transferred /
  // started_at map to NOT-NULL columns and are always present; finished_at is
  // omitempty (absent → undefined, never an explicit null).
  /** Number of retries the agent made. */
  retry_count: number
  /** Bytes transferred (partial on failure). */
  bytes_transferred: number
  /** When the upload attempt started (NOT NULL). */
  started_at: string
  /** When the upload finished; omitempty → absent (undefined) when unset. */
  finished_at?: string
  uploaded_at: string
}

/** Query parameters for listing upload logs */
export interface ListUploadLogsParams {
  agent_id?: string
  /** Only the real upload-log statuses are filterable (case-normalized server-side). */
  status?: UploadLog['status']
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
