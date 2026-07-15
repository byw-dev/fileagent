import apiClient from './api'

/** Upload log entity */
export interface UploadLog {
  id: string
  agent_id: string
  file_id: string | null
  filename: string
  size: number
  status: 'SUCCESS' | 'FAILED' | 'PENDING'
  error_message: string | null
  /** Number of retries the agent made (retry trail, 4e). */
  retry_count: number
  /** Bytes transferred so far (partial on failure). */
  bytes_transferred: number
  /** When the upload attempt started (absent for legacy rows). */
  started_at?: string
  /** When the upload finished (absent while in-flight). */
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
