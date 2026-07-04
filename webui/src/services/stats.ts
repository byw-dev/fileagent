import apiClient from './api'

/** One day of the upload trend. */
export interface DayCount {
  date: string // YYYY-MM-DD (UTC)
  count: number
}

/** Aggregate figures shown on the dashboard. */
export interface DashboardStats {
  total_agents: number
  online_agents: number
  total_files: number
  storage_bytes: number
  today_uploads: number
  upload_trend: DayCount[]
}

/**
 * Fetch real dashboard aggregates from the Control Plane, replacing the earlier
 * client-side approximations derived from a recent-logs sample.
 */
export async function getDashboardStats(): Promise<DashboardStats> {
  const response = await apiClient.get<DashboardStats>('/api/v1/stats/dashboard')
  return response.data
}
