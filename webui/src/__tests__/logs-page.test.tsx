import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import type { UploadLog } from '../services/upload-logs'
import LogsPage from '../pages/Logs'

const listUploadLogsMock = vi.fn()

vi.mock('../services/upload-logs', () => ({
  listUploadLogs: (...args: unknown[]) => listUploadLogsMock(...args),
}))

// ProTable re-runs its request whenever `params` changes (like the real one), so
// the chip filter's effect on the query can be asserted.
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({
      request,
      params,
    }: {
      request?: () => Promise<{ data?: UploadLog[] }>
      params?: Record<string, unknown>
    }) => {
      ReactModule.useEffect(() => {
        void request?.()
      }, [JSON.stringify(params)])
      return <div data-testid="mock-pro-table" />
    },
  }
})

describe('LogsPage (WR-6)', () => {
  beforeEach(() => {
    listUploadLogsMock.mockReset()
    listUploadLogsMock.mockResolvedValue({ items: [], total: 0, next_cursor: null, has_more: false })
  })

  it('renders the heading + status chips and requests logs', async () => {
    render(<LogsPage />)
    expect(screen.getByRole('heading', { name: '上传日志' })).toBeInTheDocument()
    expect(screen.getByText('全部')).toBeInTheDocument()
    expect(screen.getByText('失败')).toBeInTheDocument()
    await waitFor(() => expect(listUploadLogsMock).toHaveBeenCalledTimes(1))
    // No filter selected initially → status param omitted.
    expect(listUploadLogsMock).toHaveBeenLastCalledWith({ limit: 100 })
  })

  it('adds the status param when a chip is selected', async () => {
    render(<LogsPage />)
    await waitFor(() => expect(listUploadLogsMock).toHaveBeenCalledTimes(1))
    fireEvent.click(screen.getByText('失败'))
    await waitFor(() =>
      expect(listUploadLogsMock).toHaveBeenLastCalledWith({ limit: 100, status: 'FAILED' }),
    )
  })
})
