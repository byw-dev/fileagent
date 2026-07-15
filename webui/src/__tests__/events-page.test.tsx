import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { App } from 'antd'
import { SWRConfig } from 'swr'
import type { EventRule } from '../services/events'
import EventsPage from '../pages/Events'

const listEventRulesMock = vi.fn()
const listRuleDeliveriesMock = vi.fn()

vi.mock('../services/events', () => ({
  listEventRules: () => listEventRulesMock(),
  deleteEventRule: vi.fn(),
  updateEventRule: vi.fn(),
  createEventRule: vi.fn(),
  listRuleDeliveries: (...args: unknown[]) => listRuleDeliveriesMock(...args),
}))

// ProTable fires its request once and renders the returned rows via the columns
// (mirrors file-types-page.test.tsx), so action-column buttons are reachable.
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({
      request,
      columns,
    }: {
      request?: () => Promise<{ data?: EventRule[] }>
      columns: Array<{ render?: (v: unknown, row: EventRule) => React.ReactNode; key: string }>
    }) => {
      const [rows, setRows] = ReactModule.useState<EventRule[]>([])
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

const sampleRule: EventRule = {
  id: 'r1',
  org_id: 'o1',
  name: '文件上传通知',
  event_type: 'file_uploaded',
  filter: {},
  action_type: 'webhook',
  action_config: { url: 'https://example.com/hook' },
  enabled: true,
  created_at: '2026-07-14T08:00:00Z',
}

const renderPage = () =>
  render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <App>
        <EventsPage />
      </App>
    </SWRConfig>,
  )

describe('EventsPage (WR-5)', () => {
  beforeEach(() => {
    listEventRulesMock.mockReset()
    listRuleDeliveriesMock.mockReset()
    listEventRulesMock.mockResolvedValue([sampleRule])
    listRuleDeliveriesMock.mockResolvedValue({ items: [], total: 0, next_cursor: null, has_more: false })
  })

  it('renders the header + create button and lists rules', async () => {
    renderPage()
    expect(screen.getByRole('heading', { name: '事件规则' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /新建规则/ })).toBeInTheDocument()
    await waitFor(() => expect(listEventRulesMock).toHaveBeenCalledTimes(1))
    expect(await screen.findByRole('button', { name: '文件上传通知' })).toBeInTheDocument()
  })

  it('opens the create drawer with the webhook URL field (4b default)', async () => {
    renderPage()
    fireEvent.click(screen.getByRole('button', { name: /新建规则/ }))
    expect(await screen.findByText('新建事件规则')).toBeInTheDocument()
    // Default action is webhook → the URL field (not NATS subject) is required.
    expect(await screen.findByText('Webhook URL')).toBeInTheDocument()
    expect(screen.queryByText('NATS Subject')).not.toBeInTheDocument()
  })

  it('opens the deliveries drawer from a rule row', async () => {
    renderPage()
    const deliveriesBtn = await screen.findByRole('button', { name: '投递记录' })
    fireEvent.click(deliveriesBtn)
    expect(await screen.findByText('投递记录：文件上传通知')).toBeInTheDocument()
    await waitFor(() => expect(listRuleDeliveriesMock).toHaveBeenCalledWith('r1', { limit: 50 }))
  })
})
