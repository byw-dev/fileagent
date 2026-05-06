import apiClient from './api'

/** File type entity */
export interface FileType {
  id: string
  org_id: string
  name: string
  description: string | null
  created_at: string
}

/**
 * List all file types in the current organisation.
 */
export async function listFileTypes(): Promise<FileType[]> {
  const response = await apiClient.get<{ data: FileType[] }>('/api/v1/file-types')
  return response.data.data
}

/**
 * Get a single file type by ID.
 */
export async function getFileType(id: string): Promise<FileType> {
  const response = await apiClient.get<FileType>(`/api/v1/file-types/${id}`)
  return response.data
}

/**
 * Create a new file type.
 */
export async function createFileType(data: {
  name: string
  description?: string
}): Promise<FileType> {
  const response = await apiClient.post<FileType>('/api/v1/file-types', data)
  return response.data
}

/**
 * Update an existing file type.
 */
export async function updateFileType(
  id: string,
  data: { name: string; description?: string }
): Promise<FileType> {
  const response = await apiClient.put<FileType>(`/api/v1/file-types/${id}`, data)
  return response.data
}

/**
 * Delete a file type by ID.
 */
export async function deleteFileType(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/file-types/${id}`)
}
