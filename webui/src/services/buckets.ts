import apiClient from './api'

/** Bucket entity */
export interface Bucket {
  id: string
  org_id: string
  name: string
  /** omitempty on the backend → absent (undefined) when none. */
  description?: string
  created_at: string
}

/**
 * List all buckets in the current organisation.
 */
export async function listBuckets(): Promise<Bucket[]> {
  const response = await apiClient.get<{ items: Bucket[]; total: number }>('/api/v1/buckets')
  return response.data.items
}

/**
 * Create a new bucket (super_admin only).
 */
export async function createBucket(data: {
  name: string
  description?: string
}): Promise<Bucket> {
  const response = await apiClient.post<Bucket>('/api/v1/buckets', data)
  return response.data
}
