/**
 * Tests for the site-wide StatusBadge (§1.2 浅底泡泡 + 亮圆点 + 深调文字, contracts.md V-1).
 * Guards the "同色同词，全站唯一映射" invariant: known values map to the right
 * label, lookup is case-insensitive, domain overrides win, unknowns fall back;
 * plus the pill's tri-color (bg / deep text / bright dot).
 */
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import StatusBadge from '../components/StatusBadge'
import { statusBadgeTones } from '../theme'

describe('StatusBadge', () => {
  it('maps agent statuses to the correct labels (RUNNING ⇄ 在线)', () => {
    render(<StatusBadge status="RUNNING" />)
    expect(screen.getByText('在线')).toBeInTheDocument()
  })

  it('maps file statuses (UPLOADING/COMPLETED/FAILED)', () => {
    const { rerender } = render(<StatusBadge status="COMPLETED" />)
    expect(screen.getByText('成功')).toBeInTheDocument()
    rerender(<StatusBadge status="FAILED" />)
    expect(screen.getByText('失败')).toBeInTheDocument()
  })

  it('is case-insensitive (lower-case delivery statuses)', () => {
    render(<StatusBadge status="dead" domain="delivery" />)
    expect(screen.getByText('已终止')).toBeInTheDocument()
  })

  it('applies a domain override (delivery PENDING = 待投递, not 待审批)', () => {
    render(<StatusBadge status="pending" domain="delivery" />)
    expect(screen.getByText('待投递')).toBeInTheDocument()
    expect(screen.queryByText('待审批')).not.toBeInTheDocument()
  })

  it('falls back to the raw value with a neutral dot for unknown statuses', () => {
    const { container } = render(<StatusBadge status="WEIRD" />)
    expect(screen.getByText('WEIRD')).toBeInTheDocument()
    const dot = container.querySelector('span > span') as HTMLElement
    expect(dot.style.backgroundColor).toBe('rgb(143, 149, 158)') // #8F959E neutral
  })

  it('renders a light pill: bg + deep text + brighter dot (dot ≠ text)', () => {
    // 在线 = success tone: bg #E8F6EF / text #1B8A5A / dot #22A06B
    const { container } = render(<StatusBadge status="RUNNING" />)
    const pill = container.querySelector('span') as HTMLElement
    const dot = container.querySelector('span > span') as HTMLElement
    expect(pill.style.backgroundColor).toBe('rgb(232, 246, 239)') // #E8F6EF
    expect(pill.style.color).toBe('rgb(27, 138, 90)') // #1B8A5A deep text
    expect(dot.style.backgroundColor).toBe('rgb(34, 160, 107)') // #22A06B bright dot
    // guard the tri-color source-of-truth + dot ≠ text
    expect(statusBadgeTones.success).toEqual({ bg: '#E8F6EF', text: '#1B8A5A', dot: '#22A06B' })
    expect(statusBadgeTones.error.dot).toBe('#D64545')
  })
})
