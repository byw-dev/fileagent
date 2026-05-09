/**
 * Tests for AgentDetailPage.
 *
 * Key regression guard: the page must not throw "Modal is not defined" on mount.
 * This crashed the entire React tree (T3-2-FIX-L: Modal was removed from the
 * antd import list during the App.useApp() refactor while still being used as a
 * JSX component for the directory-browser modal at line ~347).
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { App } from 'antd'

// ── service mocks ────────────────────────────────────────────────────────────
const mockGetAgent = vi.fn()
const mockListRules = vi.fn()
const mockListAgentUploadLogs = vi.fn()

vi.mock('../services/agents', () => ({
  getAgent: (...args: unknown[]) => mockGetAgent(...args),
  approveAgent: vi.fn(),
  revokeAgent: vi.fn(),
  listRules: (...args: unknown[]) => mockListRules(...args),
  deleteRule: vi.fn(),
  listDir: vi.fn(),
  listAgentUploadLogs: (...args: unknown[]) => mockListAgentUploadLogs(...args),
}))

vi.mock('../services/upload-logs', () => ({}))

// ── antd Pro mock ─────────────────────────────────────────────────────────────
vi.mock('@ant-design/pro-components', () => ({
  ProTable: () => <div data-testid="mock-pro-table" />,
}))

// ── component under test ─────────────────────────────────────────────────────
import AgentDetailPage from '../pages/Agents/Detail'

const AGENT_ID = '3484c64d-5f68-4dc7-9ecb-d8c10084ab0c'

const sampleAgent = {
  id: AGENT_ID,
  org_id: 'org-1',
  name: 'edge-agent-01',
  status: 'APPROVED',
  hostname: 'host1',
  ip_address: '192.168.1.10',
  os_type: 'linux',
  os_version: '5.15',
  agent_version: '1.0.0',
  created_at: '2026-05-01T00:00:00Z',
}

/**
 * Wrap the detail page with the providers it depends on:
 * - MemoryRouter (for useParams / useNavigate)
 * - antd App (for App.useApp() → modal, message)
 */
function renderPage(agentId = AGENT_ID) {
  return render(
    <App>
      <MemoryRouter initialEntries={[`/agents/${agentId}`]}>
        <Routes>
          <Route path="/agents/:id" element={<AgentDetailPage />} />
        </Routes>
      </MemoryRouter>
    </App>,
  )
}

describe('AgentDetailPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockGetAgent.mockResolvedValue(sampleAgent)
    mockListRules.mockResolvedValue({ items: [], total: 0 })
    mockListAgentUploadLogs.mockResolvedValue({ items: [], total: 0 })
  })

  it('renders without crashing (Modal import regression guard)', async () => {
    // Guards against "ReferenceError: Modal is not defined" (T3-2-FIX-L):
    // if Modal is removed from the antd imports the component throws on
    // render and this test fails with an uncaught ReferenceError.
    expect(() => renderPage()).not.toThrow()
  })

  it('calls getAgent with the agent ID from the URL', async () => {
    renderPage()
    await waitFor(() => {
      expect(mockGetAgent).toHaveBeenCalledWith(AGENT_ID)
    })
  })

  it('displays agent name after data loads', async () => {
    renderPage()
    await waitFor(() => {
      // The name appears in the page heading; getAllByText handles duplicates
      const elements = screen.getAllByText('edge-agent-01')
      expect(elements.length).toBeGreaterThan(0)
    })
  })

  it('displays the Chinese label for APPROVED status', async () => {
    // AgentStatusBadge renders '已审批' for the APPROVED status value.
    renderPage()
    await waitFor(() => {
      const badges = screen.getAllByText('已审批')
      expect(badges.length).toBeGreaterThan(0)
    })
  })

  it('shows a loading spinner before data arrives', () => {
    // Never resolve — keep the promise pending to catch the loading state.
    mockGetAgent.mockReturnValue(new Promise(() => {}))
    renderPage()
    // A loading indicator (Spin) is shown while awaiting the API response.
    const spinners = document.querySelectorAll('.ant-spin')
    expect(spinners.length).toBeGreaterThan(0)
  })
})
