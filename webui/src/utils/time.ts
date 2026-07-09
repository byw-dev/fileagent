/**
 * Time formatting helpers (§ 定则 5).
 *
 * Site-wide rule: timestamps show as `MM-DD HH:mm` (local), with the full
 * datetime on hover; relative time ("x分钟前") is reserved for "last heartbeat".
 * All helpers are null/invalid-safe — an empty or unparseable input returns the
 * fallback (default `-`) instead of the `Invalid Date` string that
 * `new Date(x).toLocaleString()` produces.
 */

/** Parse an input into a valid Date, or null if empty/unparseable. */
function toDate(input?: string | number | null): Date | null {
  if (input === null || input === undefined || input === '') return null
  const d = new Date(input)
  return Number.isNaN(d.getTime()) ? null : d
}

const pad = (n: number): string => String(n).padStart(2, '0')

/** `MM-DD HH:mm` in local time. Returns `fallback` for empty/invalid input. */
export function formatTime(input?: string | number | null, fallback = '-'): string {
  const d = toDate(input)
  if (!d) return fallback
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** Full local datetime (for tooltips). Returns `fallback` for empty/invalid input. */
export function formatTimeFull(input?: string | number | null, fallback = '-'): string {
  const d = toDate(input)
  if (!d) return fallback
  return d.toLocaleString('zh-CN')
}

/**
 * Relative time like `刚刚` / `3分钟前` / `2小时前` / `5天前`. For spans beyond a
 * week (or future times) it falls back to {@link formatTime}. Empty/invalid → `fallback`.
 */
export function formatRelative(input?: string | number | null, fallback = '-'): string {
  const d = toDate(input)
  if (!d) return fallback
  const diffMs = Date.now() - d.getTime()
  if (diffMs < 0) return formatTime(input, fallback)
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return '刚刚'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}小时前`
  const day = Math.floor(hr / 24)
  if (day < 7) return `${day}天前`
  return formatTime(input, fallback)
}
