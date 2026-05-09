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
  metadata?: Record<string, string>
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
    { params }
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
