import apiClient from './api'

/** Controlled tag key (metadata 6c). */
export interface TagKey {
  id: string
  org_id: string
  key: string
  label: string
  value_controlled: boolean
  required_at_collection: boolean
  allow_path_var: boolean
  system_reserved: boolean
  created_at: string
}

/** A legal value for a controlled tag key. */
export interface TagValue {
  id: string
  value: string
}

/** Fields settable when creating a tag key. */
export interface CreateTagKeyInput {
  key: string
  label: string
  value_controlled?: boolean
  required_at_collection?: boolean
  allow_path_var?: boolean
}

/** Fields settable when updating a tag key (key itself is immutable). */
export interface UpdateTagKeyInput {
  label?: string
  value_controlled?: boolean
  required_at_collection?: boolean
  allow_path_var?: boolean
}

/** List all tag keys in the current organisation. */
export async function listTagKeys(): Promise<TagKey[]> {
  const response = await apiClient.get<{ items: TagKey[]; total: number }>('/api/v1/tag-keys')
  return response.data.items
}

/** Create a new tag key (super_admin). */
export async function createTagKey(data: CreateTagKeyInput): Promise<TagKey> {
  const response = await apiClient.post<TagKey>('/api/v1/tag-keys', data)
  return response.data
}

/** Update an existing tag key by its key name (super_admin). */
export async function updateTagKey(key: string, data: UpdateTagKeyInput): Promise<TagKey> {
  const response = await apiClient.patch<TagKey>(`/api/v1/tag-keys/${encodeURIComponent(key)}`, data)
  return response.data
}

/** Delete a tag key by its key name (super_admin). */
export async function deleteTagKey(key: string): Promise<void> {
  await apiClient.delete(`/api/v1/tag-keys/${encodeURIComponent(key)}`)
}

/** List the controlled values of a tag key. */
export async function listTagValues(key: string): Promise<TagValue[]> {
  const response = await apiClient.get<{ items: TagValue[]; total: number }>(
    `/api/v1/tag-keys/${encodeURIComponent(key)}/values`,
  )
  return response.data.items
}

/** Add a legal value to a tag key (super_admin). */
export async function createTagValue(key: string, value: string): Promise<TagValue> {
  const response = await apiClient.post<TagValue>(
    `/api/v1/tag-keys/${encodeURIComponent(key)}/values`,
    { value },
  )
  return response.data
}

/** Delete a value from a tag key by value id (super_admin). */
export async function deleteTagValue(key: string, valueId: string): Promise<void> {
  await apiClient.delete(`/api/v1/tag-keys/${encodeURIComponent(key)}/values/${valueId}`)
}
