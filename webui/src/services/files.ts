import apiClient from './api'

/** File entry entity */
export interface FileEntry {
  id: string
  agent_id: string
  file_type_id: string | null
  bucket_id: string
  original_path: string
  storage_key: string
  filename: string
  size: number
  sha256: string
  mime_type: string
  status: 'PENDING' | 'INDEXED' | 'ERROR'
  uploaded_at: string
  indexed_at?: string | null
  /** Controlled tags attached to the file (metadata 6c), key → value. */
  tags?: Record<string, string>
}

/** Query parameters for listing files */
export interface ListFilesParams {
  agent_id?: string
  file_type_id?: string
  bucket_id?: string
  status?: string
  cursor?: string
  limit?: number
  filename?: string
  /** Repeatable key:value tag predicates, AND-combined (?tag=site:tokyo&tag=…). */
  tag?: string[]
}

/** Paginated list response */
export interface PaginatedResponse<T> {
  items: T[]
  total: number
  next_cursor: string | null
  has_more: boolean
}

/**
 * List file entries with optional filtering (cursor-based pagination).
 */
export async function listFiles(
  params?: ListFilesParams
): Promise<PaginatedResponse<FileEntry>> {
  const response = await apiClient.get<PaginatedResponse<FileEntry>>(
    '/api/v1/files',
    // indexes: null repeats array params as `tag=a&tag=b` (no `[]`), which is what
    // the backend's repeatable `tag` query parameter expects.
    { params, paramsSerializer: { indexes: null } }
  )
  return response.data
}

/** Selection predicate for batch tagging — mirrors the GET /files filter. */
export interface BatchTagFilter {
  agent_id?: string
  bucket_id?: string
  file_type_id?: string
  status?: string
  tags?: string[]
}

/**
 * Bulk set/clear tags on every file matching a GET /files-style predicate
 * (super_admin). A tag value is a string (set) or null (clear). Runs
 * asynchronously on the retag worker; returns the enqueued job id.
 */
export async function batchTagFiles(
  filter: BatchTagFilter,
  tags: Record<string, string | null>
): Promise<{ job_id: string; status: string }> {
  const response = await apiClient.post<{ job_id: string; status: string }>(
    '/api/v1/files/batch-tag',
    { filter, tags }
  )
  return response.data
}

/**
 * Get a single file entry by ID.
 */
export async function getFile(id: string): Promise<FileEntry> {
  const response = await apiClient.get<FileEntry>(`/api/v1/files/${id}`)
  return response.data
}

/**
 * Get a presigned download URL for a file.
 */
export async function getFileDownloadUrl(id: string): Promise<{ url: string }> {
  const response = await apiClient.get<{ url: string }>(
    `/api/v1/files/${id}/download-url`
  )
  return response.data
}

/**
 * Delete a file entry.
 */
export async function deleteFile(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/files/${id}`)
}
