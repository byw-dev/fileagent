/**
 * Tests for time formatting helpers (§ 定则 5).
 * Guards the MM-DD HH:mm format, relative buckets, and the null/invalid safety
 * that prevents the `Invalid Date` bug seen on the Files page.
 */
import { describe, it, expect, vi, afterEach } from 'vitest'
import { formatTime, formatTimeFull, formatRelative } from '../utils/time'

describe('formatTime', () => {
  it('formats a timestamp as MM-DD HH:mm (local)', () => {
    // Construct via local components so the test is timezone-independent.
    const d = new Date(2026, 6, 9, 4, 38) // 2026-07-09 04:38 local
    expect(formatTime(d.toISOString())).toBe('07-09 04:38')
  })

  it('zero-pads month/day/hour/minute', () => {
    const d = new Date(2026, 0, 3, 9, 5) // 2026-01-03 09:05 local
    expect(formatTime(d.toISOString())).toBe('01-03 09:05')
  })

  it.each([null, undefined, '', 'not-a-date'])(
    'returns fallback for empty/invalid input (%s)',
    (input) => {
      expect(formatTime(input as string | null)).toBe('-')
      expect(formatTime(input as string | null, '—')).toBe('—')
    },
  )
})

describe('formatTimeFull', () => {
  it('returns a non-empty locale string for a valid date', () => {
    expect(formatTimeFull(new Date(2026, 6, 9).toISOString())).not.toBe('-')
  })
  it('returns fallback for invalid input', () => {
    expect(formatTimeFull('nope')).toBe('-')
  })
})

describe('formatRelative', () => {
  afterEach(() => vi.useRealTimers())

  const withNow = (now: Date, fn: () => void) => {
    vi.useFakeTimers()
    vi.setSystemTime(now)
    fn()
  }

  it('buckets recent times', () => {
    const now = new Date(2026, 6, 9, 12, 0, 0)
    withNow(now, () => {
      expect(formatRelative(new Date(2026, 6, 9, 11, 59, 30).toISOString())).toBe('刚刚')
      expect(formatRelative(new Date(2026, 6, 9, 11, 57, 0).toISOString())).toBe('3分钟前')
      expect(formatRelative(new Date(2026, 6, 9, 10, 0, 0).toISOString())).toBe('2小时前')
      expect(formatRelative(new Date(2026, 6, 4, 12, 0, 0).toISOString())).toBe('5天前')
    })
  })

  it('falls back to MM-DD HH:mm beyond a week', () => {
    const now = new Date(2026, 6, 20, 12, 0, 0)
    withNow(now, () => {
      expect(formatRelative(new Date(2026, 6, 9, 4, 38, 0).toISOString())).toBe('07-09 04:38')
    })
  })

  it('returns fallback for empty/invalid input', () => {
    expect(formatRelative(null)).toBe('-')
  })
})
