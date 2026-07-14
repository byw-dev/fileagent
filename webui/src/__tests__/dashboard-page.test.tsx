import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import DashboardPage from '../pages/Dashboard'

const getDashboardStatsMock = vi.fn()
const listAgentsMock = vi.fn()
const listUploadLogsMock = vi.fn()

vi.mock('../services/stats', () => ({
  getDashboardStats: () => getDashboardStatsMock(),
}))
vi.mock('../services/agents', () => ({
  listAgents: () => listAgentsMock(),
}))
vi.mock('../services/upload-logs', () => ({
  listUploadLogs: () => listUploadLogsMock(),
}))

// recharts relies on layout measurement that jsdom lacks; stub to plain divs so
// the smoke test focuses on the page shell (cards / skeleton / tables).
vi.mock('recharts', () => {
  const Stub = ({ children }: { children?: React.ReactNode }) => <div>{children}</div>
  return {
    ResponsiveContainer: Stub,
    LineChart: Stub,
    Line: Stub,
    XAxis: Stub,
    YAxis: Stub,
    CartesianGrid: Stub,
    Tooltip: Stub,
  }
})

// Fresh SWR cache per render so tests don't share fetched state.
const renderPage = () =>
  render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <DashboardPage />
    </SWRConfig>,
  )

describe('DashboardPage (WR-9)', () => {
  beforeEach(() => {
    getDashboardStatsMock.mockReset()
    listAgentsMock.mockReset()
    listUploadLogsMock.mockReset()
    getDashboardStatsMock.mockResolvedValue({
      total_agents: 3,
      online_agents: 2,
      total_files: 42,
      storage_bytes: 1024,
      today_uploads: 5,
      upload_trend: [
        { date: '2026-07-13', count: 1 },
        { date: '2026-07-14', count: 4 },
      ],
    })
    listAgentsMock.mockResolvedValue({ items: [] })
    listUploadLogsMock.mockResolvedValue({ items: [] })
  })

  it('renders the heading and requests server-side aggregates', async () => {
    renderPage()
    expect(screen.getByRole('heading', { name: '仪表盘' })).toBeInTheDocument()
    await waitFor(() => expect(getDashboardStatsMock).toHaveBeenCalledTimes(1))
    expect(listAgentsMock).toHaveBeenCalledTimes(1)
    expect(listUploadLogsMock).toHaveBeenCalledTimes(1)
  })

  it('shows stat cards once aggregates resolve (skeleton → content)', async () => {
    renderPage()
    // Card loading renders a skeleton first; titles appear after data settles.
    expect(await screen.findByText('在线采集器')).toBeInTheDocument()
    expect(screen.getByText('今日上传')).toBeInTheDocument()
    expect(screen.getByText('总文件数')).toBeInTheDocument()
    expect(screen.getByText('存储用量')).toBeInTheDocument()
  })
})
