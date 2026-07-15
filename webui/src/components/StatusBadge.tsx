import { statusBadgeTones } from '../theme'

/**
 * StatusBadge — the site-wide status indicator: a light-tinted pill with a
 * brighter dot + deeper label (§1.2 浅底泡泡 + 亮圆点 + 深调文字). One value maps
 * to one color + one word everywhere ("同色同词，全站唯一映射").
 *
 * Status values follow the REST contract (docs/design/contracts.md V-1):
 * agent / file / upload-log statuses arrive UPPER-CASE from the API; event
 * delivery statuses arrive lower-case. Lookup is case-insensitive.
 */

type Tone = keyof typeof statusBadgeTones // 'success' | 'warning' | 'error' | 'neutral' | 'processing'

interface Meta {
  label: string
  tone: Tone
}

/** Shared map keyed by UPPER-CASE status value. */
const BASE: Record<string, Meta> = {
  // Agent (V-1: RUNNING ⇄ DB online)
  PENDING: { label: '待审批', tone: 'warning' },
  APPROVED: { label: '已审批', tone: 'processing' },
  RUNNING: { label: '在线', tone: 'success' },
  OFFLINE: { label: '离线', tone: 'neutral' },
  REVOKED: { label: '已吊销', tone: 'error' },
  // File / upload-log (ToUpper at REST boundary): completed/failed/uploading.
  UPLOADING: { label: '上传中', tone: 'processing' },
  COMPLETED: { label: '成功', tone: 'success' },
  FAILED: { label: '失败', tone: 'error' },
  DELETED: { label: '已删除', tone: 'neutral' },
  // Collection-rule status
  ACTIVE: { label: '生效', tone: 'success' },
  INACTIVE: { label: '已停用', tone: 'neutral' },
  // Event delivery lifecycle (CC-7): pending → delivered / failed → dead
  DELIVERED: { label: '已投递', tone: 'success' },
  DEAD: { label: '已终止', tone: 'error' },
}

/** Domain of the status value — refines the few values that collide across domains. */
export type StatusDomain = 'agent' | 'file' | 'upload' | 'rule' | 'delivery'

/** Per-domain overrides for ambiguous values (e.g. delivery PENDING = 待投递, not 待审批). */
const OVERRIDE: Partial<Record<StatusDomain, Record<string, Meta>>> = {
  delivery: {
    PENDING: { label: '待投递', tone: 'processing' },
  },
}

interface StatusBadgeProps {
  /** Status value from the API (case-insensitive). */
  status?: string | null
  /** Optional domain to disambiguate values shared across resources. */
  domain?: StatusDomain
}

/**
 * StatusBadge — renders a colored dot + localized label for any known status.
 * Unknown values fall back to the raw string with a neutral dot.
 */
function StatusBadge({ status, domain }: StatusBadgeProps) {
  const key = String(status ?? '').toUpperCase()
  const meta: Meta =
    (domain && OVERRIDE[domain]?.[key]) ??
    BASE[key] ?? { label: status ? String(status) : '-', tone: 'neutral' }
  const tone = statusBadgeTones[meta.tone]

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        height: 22,
        padding: '0 9px',
        borderRadius: 11,
        fontSize: '0.75rem',
        lineHeight: 1,
        whiteSpace: 'nowrap',
        backgroundColor: tone.bg,
        color: tone.text,
      }}
    >
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: '50%',
          backgroundColor: tone.dot,
          flex: 'none',
        }}
      />
      {meta.label}
    </span>
  )
}

export default StatusBadge
