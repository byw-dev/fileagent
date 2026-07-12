import apiClient from './api'

/** A value that path extraction / manual tagging saw but that is not yet in a
 * controlled key's vocabulary, awaiting admin review (metadata 6c). */
export interface PendingTagValue {
  id: string
  key: string
  extracted_value: string
  source: string
  hit_count: number
  suggested_value?: string
  first_seen_at: string
}

/** Result of enqueueing a merge (runs asynchronously on the retag worker). */
export interface MergeResult {
  job_id: string
  status: string
  into: string
}

/** List the pending tag-value review queue (any authenticated user). */
export async function listPendingTagValues(): Promise<PendingTagValue[]> {
  const response = await apiClient.get<{ items: PendingTagValue[]; total: number }>(
    '/api/v1/pending-tag-values',
  )
  return response.data.items
}

/** Approve a pending value: promote it into the key's vocabulary (super_admin). */
export async function approvePendingTagValue(id: string): Promise<void> {
  await apiClient.post(`/api/v1/pending-tag-values/${id}/approve`)
}

/**
 * Merge a pending value into an existing canonical value (super_admin). Rewrites
 * already-tagged files asynchronously; returns the retag job id. `into` defaults
 * to the row's suggested value when omitted.
 */
export async function mergePendingTagValue(id: string, into?: string): Promise<MergeResult> {
  const response = await apiClient.post<MergeResult>(
    `/api/v1/pending-tag-values/${id}/merge`,
    into ? { into } : {},
  )
  return response.data
}

/** Reject a pending value: drop it from the queue without adding it (super_admin). */
export async function rejectPendingTagValue(id: string): Promise<void> {
  await apiClient.post(`/api/v1/pending-tag-values/${id}/reject`)
}
