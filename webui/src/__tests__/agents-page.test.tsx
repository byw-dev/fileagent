import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App } from 'antd'
import { SWRConfig } from 'swr'
import { MemoryRouter } from 'react-router-dom'
import type { Agent, ListAgentsParams } from '../services/agents'
import AgentsPage from '../pages/Agents'

const listAgentsMock = vi.fn()

vi.mock('../services/agents', () => ({
  listAgents: (p?: ListAgentsParams) => listAgentsMock(p),
  approveAgent: vi.fn(),
  revokeAgent: vi.fn(),
}))

// ProTable fires request once and renders rows through the columns so the status
// column (effective badge) is reachable (mirrors events-page.test.tsx).
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({
      request,
      columns,
    }: {
      request?: () => Promise<{ data?: Agent[] }>
      columns: Array<{ render?: (v: unknown, row: Agent) => React.ReactNode; key: string }>
    }) => {
      const [rows, setRows] = ReactModule.useState<Agent[]>([])
      const requested = ReactModule.useRef(false)
      if (!requested.current) {
        requested.current = true
        void request?.().then((r) => setRows(r.data ?? []))
      }
      return (
        <div data-testid="mock-pro-table">
          {rows.map((row) => (
            <div key={row.id}>
              {columns.map((c) => (
                <span key={c.key}>{c.render ? c.render(undefined, row) : null}</span>
              ))}
            </div>
          ))}
        </div>
      )
    },
  }
})

const agent = (over: Partial<Agent>): Agent => ({
  id: 'a1',
  name: 'sensor-01',
  hostname: 'host-1',
  ip_address: '10.0.0.1',
  status: 'RUNNING',
  is_online: true,
  os_type: 'linux',
  os_version: '1',
  agent_version: '1.0',
  last_seen_at: '2026-07-14T08:00:00Z',
  created_at: '2026-07-10T08:00:00Z',
  org_id: 'o1',
  ...over,
})

const renderPage = () =>
  render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <MemoryRouter>
        <App>
          <AgentsPage />
        </App>
      </MemoryRouter>
    </SWRConfig>,
  )

describe('AgentsPage (WR-3)', () => {
  beforeEach(() => {
    listAgentsMock.mockReset()
  })

  it('shows the 待审批置顶横幅 when there are pending agents', async () => {
    listAgentsMock.mockImplementation((p?: ListAgentsParams) =>
      p?.status === 'PENDING'
        ? Promise.resolve({ items: [], total: 3, next_cursor: null, has_more: false })
        : Promise.resolve({ items: [], total: 0, next_cursor: null, has_more: false }),
    )
    renderPage()
    expect(await screen.findByText('有 3 个采集器待审批')).toBeInTheDocument()
  })

  it('hides the banner when nothing is pending', async () => {
    listAgentsMock.mockResolvedValue({ items: [], total: 0, next_cursor: null, has_more: false })
    renderPage()
    await waitFor(() => expect(listAgentsMock).toHaveBeenCalled())
    expect(screen.queryByText(/待审批/)).not.toBeInTheDocument()
  })

  it('renders 离线 for a RUNNING agent whose real-time is_online is false (caliber fix)', async () => {
    listAgentsMock.mockImplementation((p?: ListAgentsParams) =>
      p?.status === 'PENDING'
        ? Promise.resolve({ items: [], total: 0, next_cursor: null, has_more: false })
        : Promise.resolve({
            items: [agent({ status: 'RUNNING', is_online: false })],
            total: 1,
            next_cursor: null,
            has_more: false,
          }),
    )
    renderPage()
    // DB status is RUNNING but the heartbeat expired → badge must read 离线.
    expect(await screen.findByText('离线')).toBeInTheDocument()
    expect(screen.queryByText('在线')).not.toBeInTheDocument()
  })
})
