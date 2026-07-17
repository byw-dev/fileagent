import apiClient from './api'

/** User entity returned by the management endpoints */
export interface ManagedUser {
  id: string
  org_id: string
  username: string
  email: string | null
  role: string
  is_active: boolean
  created_at: string
}

/**
 * List all users in the organisation (super_admin only).
 */
export async function listUsers(): Promise<ManagedUser[]> {
  const response = await apiClient.get<{ items: ManagedUser[]; total: number }>('/api/v1/users')
  return response.data.items
}

/**
 * Create a new user (super_admin only).
 */
export async function createUser(data: {
  username: string
  email?: string
  password: string
  role: string
}): Promise<ManagedUser> {
  const response = await apiClient.post<ManagedUser>('/api/v1/users', data)
  return response.data
}

/**
 * Update an existing user's profile (super_admin only).
 */
export async function updateUser(
  id: string,
  data: { username?: string; email?: string; role?: string }
): Promise<ManagedUser> {
  const response = await apiClient.put<ManagedUser>(`/api/v1/users/${id}`, data)
  return response.data
}

/**
 * Enable or disable a user (super_admin only). Soft alternative to delete
 * (5b「禁用而非删除」) — the caller cannot disable their own account.
 */
export async function setUserActive(id: string, isActive: boolean): Promise<ManagedUser> {
  const response = await apiClient.put<ManagedUser>(`/api/v1/users/${id}/active`, {
    is_active: isActive,
  })
  return response.data
}

/**
 * Delete a user (super_admin only). Retained for completeness; the UI prefers
 * {@link setUserActive} (禁用而非删除).
 */
export async function deleteUser(id: string): Promise<void> {
  await apiClient.delete(`/api/v1/users/${id}`)
}

/**
 * Update a user's password. Super_admin can update any user; regular users
 * can only update their own password.
 */
export async function updateUserPassword(
  id: string,
  password: string
): Promise<void> {
  await apiClient.put(`/api/v1/users/${id}/password`, { password })
}
