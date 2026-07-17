import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { App } from 'antd'
import { SWRConfig } from 'swr'
import type { FileEntry } from '../services/files'
import FilesPage from '../pages/Files'

const listFilesMock = vi.fn()
const getFileMock = vi.fn()

let currentUser: { id: string; username: string; role: string } | null = null

vi.mock('../store/auth', () => ({
  default: (selector: (state: { user: typeof currentUser }) => unknown) =>
    selector({ user: currentUser }),
}))

vi.mock('../services/files', () => ({
  listFiles: (...a: unknown[]) => listFilesMock(...a),
  getFile: (id: string) => getFileMock(id),
  getFileDownloadUrl: vi.fn(),
  batchTagFiles: vi.fn(),
}))

vi.mock('../services/tags', () => ({
  listTagKeys: () => Promise.resolve([]),
  listTagValues: () => Promise.resolve([]),
}))

// ProTable fires request once and renders rows through the columns so the
// filename button (opens the detail drawer) and status badge are reachable.
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({
      request,
      columns,
    }: {
      request?: () => Promise<{ data?: FileEntry[] }>
      columns: Array<{ render?: (v: unknown, row: FileEntry) => React.ReactNode; key: string }>
    }) => {
      const [rows, setRows] = ReactModule.useState<FileEntry[]>([])
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

const sampleFile: FileEntry = {
  id: 'f1',
  agent_id: 'a1',
  file_type_id: null,
  bucket_id: 'b1',
  original_path: '/data/x.parquet',
  storage_key: 'sensor/x.parquet',
  filename: 'x.parquet',
  size: 2048,
  sha256: 'abc',
  mime_type: 'application/octet-stream',
  status: 'COMPLETED',
  uploaded_at: '2026-07-14T08:00:00Z',
  indexed_at: null,
  tags: { site: 'tokyo' },
}

const renderPage = () =>
  render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <App>
        <FilesPage />
      </App>
    </SWRConfig>,
  )

describe('FilesPage (WR-4)', () => {
  beforeEach(() => {
    listFilesMock.mockReset()
    getFileMock.mockReset()
    currentUser = { id: 'u1', username: 'viewer', role: 'admin' }
    listFilesMock.mockResolvedValue({ items: [sampleFile], total: 1, next_cursor: null, has_more: false })
    getFileMock.mockResolvedValue(sampleFile)
  })

  it('renders the list and requests files', async () => {
    renderPage()
    expect(screen.getByRole('heading', { name: '文件浏览器' })).toBeInTheDocument()
    await waitFor(() => expect(listFilesMock).toHaveBeenCalled())
    expect(await screen.findByRole('button', { name: 'x.parquet' })).toBeInTheDocument()
  })

  it('opens the detail drawer when a filename is clicked', async () => {
    renderPage()
    const nameBtn = await screen.findByRole('button', { name: 'x.parquet' })
    fireEvent.click(nameBtn)
    await waitFor(() => expect(getFileMock).toHaveBeenCalledWith('f1'))
    // Drawer renders metadata labels once the file resolves.
    expect(await screen.findByText('存储路径')).toBeInTheDocument()
  })

  it('hides 批量打标 for non-super_admin', async () => {
    renderPage()
    await waitFor(() => expect(listFilesMock).toHaveBeenCalled())
    expect(screen.queryByRole('button', { name: /批量打标/ })).not.toBeInTheDocument()
  })
})
