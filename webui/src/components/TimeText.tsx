import { Tooltip } from 'antd'
import { formatTime, formatTimeFull, formatRelative } from '../utils/time'

interface TimeTextProps {
  /** ISO timestamp (or epoch) from the API. */
  value?: string | number | null
  /** Show relative time ("3分钟前") instead of `MM-DD HH:mm` — for "last heartbeat". */
  relative?: boolean
  /** Text shown when the value is empty/invalid (default `-`). */
  fallback?: string
}

/**
 * TimeText — renders a timestamp as `MM-DD HH:mm` (or relative when `relative`),
 * with the full datetime on hover (§ 定则 5). Null/invalid-safe.
 */
function TimeText({ value, relative = false, fallback = '-' }: TimeTextProps) {
  const display = relative ? formatRelative(value, fallback) : formatTime(value, fallback)
  if (display === fallback) return <>{fallback}</>
  // Wrap in a <span> so Tooltip has a real element child to attach refs/handlers to.
  return (
    <Tooltip title={formatTimeFull(value, fallback)}>
      <span>{display}</span>
    </Tooltip>
  )
}

export default TimeText
