import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { App } from 'antd'
import type { FileType } from '../services/file-types'
import FileTypesPage from '../pages/FileTypes'

// FileTypesPage calls App.useApp() for message; wrap with antd <App> so that
// context exists (mirrors agent-detail.test.tsx / rule-form-edit.test.tsx).
const renderPage = () =>
  render(
    <App>
      <FileTypesPage />
    </App>,
  )

const listFileTypesMock = vi.fn()

vi.mock('../services/file-types', () => ({
  listFileTypes: () => listFileTypesMock(),
  createFileType: vi.fn(),
  updateFileType: vi.fn(),
  deleteFileType: vi.fn(),
}))

// ProTable is mocked to fire its request once (data rendering is not needed for
// these smoke assertions), mirroring settings-pages.test.tsx.
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({ request }: { request?: () => Promise<{ data?: FileType[] }> }) => {
      const requested = ReactModule.useRef(false)
      if (!requested.current) {
        requested.current = true
        void request?.()
      }
      return <div data-testid="mock-pro-table" />
    },
  }
})

describe('FileTypesPage (WR-2)', () => {
  beforeEach(() => {
    listFileTypesMock.mockReset()
    listFileTypesMock.mockResolvedValue([])
  })

  it('renders the header + create button and requests the list', async () => {
    renderPage()
    expect(screen.getByRole('heading', { name: '文件类型' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /新建类型/ })).toBeInTheDocument()
    await waitFor(() => expect(listFileTypesMock).toHaveBeenCalledTimes(1))
  })

  it('opens the create FormDrawer on 新建类型', () => {
    renderPage()
    fireEvent.click(screen.getByRole('button', { name: /新建类型/ }))
    // FormDrawer (portal) renders the create title.
    expect(screen.getByText('新建文件类型')).toBeInTheDocument()
  })
})
