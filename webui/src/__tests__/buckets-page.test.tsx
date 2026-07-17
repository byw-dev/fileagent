import { describe, it, expect, beforeEach, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { App } from 'antd'
import type { Bucket } from '../services/buckets'
import BucketsPage from '../pages/Buckets'

const listBucketsMock = vi.fn()

let currentUser: { id: string; username: string; role: string } | null = null

vi.mock('../store/auth', () => ({
  default: (selector: (state: { user: typeof currentUser }) => unknown) =>
    selector({ user: currentUser }),
}))

vi.mock('../services/buckets', () => ({
  listBuckets: () => listBucketsMock(),
  createBucket: vi.fn(),
}))

// ProTable fires its request once (data rendering not needed for these smoke
// assertions), mirroring file-types-page.test.tsx.
vi.mock('@ant-design/pro-components', async () => {
  const ReactModule = await import('react')
  return {
    ProTable: ({ request }: { request?: () => Promise<{ data?: Bucket[] }> }) => {
      const requested = ReactModule.useRef(false)
      if (!requested.current) {
        requested.current = true
        void request?.()
      }
      return <div data-testid="mock-pro-table" />
    },
  }
})

const renderPage = () =>
  render(
    <App>
      <BucketsPage />
    </App>,
  )

describe('BucketsPage (WR-7)', () => {
  beforeEach(() => {
    listBucketsMock.mockReset()
    listBucketsMock.mockResolvedValue([])
    currentUser = null
  })

  it('lists buckets and hides 新建 for non-super_admin', async () => {
    currentUser = { id: 'u1', username: 'viewer', role: 'admin' }
    renderPage()
    expect(screen.getByRole('heading', { name: 'Bucket 管理' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /新建 Bucket/ })).not.toBeInTheDocument()
    await waitFor(() => expect(listBucketsMock).toHaveBeenCalledTimes(1))
  })

  it('super_admin can open the create drawer', async () => {
    currentUser = { id: 'u0', username: 'root', role: 'super_admin' }
    renderPage()
    const createBtn = screen.getByRole('button', { name: /新建 Bucket/ })
    fireEvent.click(createBtn)
    // FormDrawer renders via a portal and mounts asynchronously.
    expect(await screen.findByText('名称')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('例如：logs-bucket')).toBeInTheDocument()
  })
})
