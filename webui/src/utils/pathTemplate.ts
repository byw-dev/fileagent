/**
 * System template variables that are always available for upload path configuration.
 * These are injected by the Agent via InjectContext and buildStoragePath.
 * IC-BUG-50 / D-034: {submit_time} is the declared reserved word for the
 * file's submit-for-upload instant (UTC); {time} is its deprecated alias.
 * Review P1-B: the list shows the LDML form on purpose — a bare reserved time
 * field ({submit_time} / {time} without a format) can never compose (Go types
 * format-less fields as string), so the UI must not advertise it.
 */
export const SYSTEM_TEMPLATE_VARIABLES: ReadonlyArray<{ key: string; desc: string }> = [
  { key: '{agent_name}',            desc: 'Agent 名称，例如 prod-sensor-01' },
  { key: '{agent_id}',              desc: 'Agent UUID，例如 a1b2c3...' },
  { key: '{filename}',              desc: '原始文件名（含扩展名），例如 data.csv' },
  { key: '{ext}',                   desc: '文件扩展名（不含点），例如 csv' },
  { key: '{submit_time:yyyy/MM/dd}', desc: '该文件被提交上传的时刻（UTC），例如 2026/09/13；必须带 LDML 时间格式' },
  { key: '{time:yyyy/MM/dd}',       desc: '已废弃（deprecated）：{submit_time} 的旧名，仍可渲染，请改用 {submit_time:yyyy/MM/dd}' },
]

/**
 * Strip the leading "/" from a path template.
 *
 * Mirror of `pkg/trollsift.NormalizeTemplate` (Go side is authoritative). All
 * leading separators are removed, not just one. An
 * object key never starts with "/", so any preview or comparison must apply the
 * same normalisation the Agent applies when composing the key.
 * See docs/design/contracts.md V-3.
 */
export function normalizeTemplate(template: string): string {
  return template.replace(/^\/+/, '')
}

/**
 * Format a LDML pattern string using the given UTC date via a single-pass substitution.
 *
 * Supported symbols (longest match wins): yyyy, yy, MM, dd, HH, mm, ss.
 * Any other characters in the LDML string are passed through unchanged.
 * The `|tz=...` suffix must be stripped by the caller before invoking this function.
 */
function formatLDML(ldml: string, now: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  const tokens: Record<string, string> = {
    yyyy: String(now.getUTCFullYear()),
    yy:   String(now.getUTCFullYear()).slice(-2),
    MM:   pad(now.getUTCMonth() + 1),
    dd:   pad(now.getUTCDate()),
    HH:   pad(now.getUTCHours()),
    mm:   pad(now.getUTCMinutes()),
    ss:   pad(now.getUTCSeconds()),
  }
  // Single-pass replacement — longer tokens (yyyy) are listed before shorter ones (yy)
  // in the alternation so the regex engine matches the longest possible token first.
  return ldml.replace(/yyyy|yy|MM|dd|HH|mm|ss/g, (tok) => tokens[tok] ?? tok)
}

/**
 * Render a preview of a path template by substituting variables with example values.
 *
 * Substitution priority (highest first):
 *  1. Fields present in `dynamicFields` — shown as `«fieldname»` (runtime
 *     placeholder). Checked FIRST, also for `{name:LDML}` fields: with
 *     parse-first priority (D-034 / review P2-C) the upload would use the
 *     parsed value, so the preview must not show the current time.
 *  2. `{fieldname:LDML}` or `{fieldname:LDML|tz=...}` — formatted with UTC current time.
 *  3. System variables (`{agent_name}`, `{agent_id}`, `{filename}`, `{ext}`) — fixed examples.
 *     Reserved time words ({submit_time}, deprecated {time}) are NOT listed
 *     here: their bare form is forbidden (cannot compose — review P1-B) and
 *     their LDML form is handled by rule 2.
 *  4. All other variables — left unchanged (valid syntax, resolved at runtime).
 *
 * A leading "/" is stripped first, matching how the Agent builds the object key.
 *
 * @param template - Path template string, e.g. `/{agent_name}/{submit_time:yyyy/MM/dd}/{filename}`.
 * @param dynamicFields - Field names extracted from path_pattern (shown as «name» placeholders).
 * @returns Preview string with example substitutions applied, without a leading "/".
 */
export function renderPathPreview(template: string, dynamicFields?: string[]): string {
  if (!template) return template

  // Mirror of pkg/trollsift.NormalizeTemplate: an object key never starts with
  // "/", so a preview that shows one does not match what actually lands in
  // MinIO. See docs/design/contracts.md V-3.
  template = normalizeTemplate(template)

  const now = new Date()

const systemSubs: Record<string, string> = {
    '{agent_name}': 'my-agent',
    '{agent_id}':   'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
    '{filename}':   'data.csv',
    '{ext}':        'csv',
  }

  const dynamicSet = new Set(dynamicFields ?? [])

  // Cross-alias mirror (review P1-A/B2, D-034): must match
  // trollsift.InjectSubmitTime on the Go side. When exactly ONE reserved word
  // was parsed out of path_pattern, the missing alias composes with the SAME
  // parsed value, so the preview shows the SOURCE field's name for both
  // spellings; when both were parsed, each keeps its own value; when neither
  // was parsed, the LDML branch renders the current time (the injected value).
  const hasLegacy = dynamicSet.has('time')
  const hasSubmit = dynamicSet.has('submit_time')
  const legacyLabel = hasLegacy ? 'time' : hasSubmit ? 'submit_time' : null
  const submitLabel = hasSubmit ? 'submit_time' : hasLegacy ? 'time' : null

  return template.replace(/\{([^}]+)\}/g, (match, inner: string) => {
    const colonIdx = inner.indexOf(':')
    const fieldName = colonIdx !== -1 ? inner.slice(0, colonIdx) : inner
    if (fieldName === 'submit_time' && submitLabel) {
      return `\u00AB${submitLabel}\u00BB`
    }
    if (fieldName === 'time' && legacyLabel) {
      return `\u00AB${legacyLabel}\u00BB`
    }
    // Review P2-C: a field path_pattern parses (dynamicSet) must win over the
    // current-time rendering — with parse-first priority (D-034) the upload
    // would use the parsed value, so showing the current time here would lie.
    if (dynamicSet.has(fieldName)) {
      return `\u00AB${fieldName}\u00BB`
    }
    if (colonIdx !== -1) {
      // Time field: {fieldname:LDML} or {fieldname:LDML|tz=...}
      let ldmlPart = inner.slice(colonIdx + 1)
      const pipeIdx = ldmlPart.indexOf('|')
      if (pipeIdx !== -1) {
        ldmlPart = ldmlPart.slice(0, pipeIdx)
      }
      return formatLDML(ldmlPart, now)
    }
    if (Object.hasOwn(systemSubs, match)) {
      return systemSubs[match]
    }
    return match
  })
}

/**
 * Validate that a path template has valid trollsift syntax.
 * No whitelist check — any field name is accepted; custom fields from
 * path_pattern are resolved at runtime.
 *
 * @param template - The template string to validate.
 * @returns An error message string, or null if the template is valid.
 */
export function validatePathTemplate(template: string): string | null {
  if (!template) return '路径模板不能为空'
  // A leading "/" is accepted but not required: it is stripped before the key is
  // built (see normalizeTemplate), so demanding it forced users to type a
  // character that never reaches the object key.
  if (template.includes('//')) return '路径模板不能包含连续的 //'

  // Check balanced braces (no nesting, all opened braces must be closed)
  let depth = 0
  for (let i = 0; i < template.length; i++) {
    if (template[i] === '{') {
      depth++
      if (depth > 1) return '模板括号不平衡'
    } else if (template[i] === '}') {
      depth--
      if (depth < 0) return '模板括号不平衡'
    }
  }
  if (depth !== 0) return '模板括号不平衡'

  // Check for empty field names and empty tz= values
  const variablePattern = /\{([^}]*)\}/g
  let m: RegExpExecArray | null
  while ((m = variablePattern.exec(template)) !== null) {
    const inner = m[1]
    if (!inner) return '模板变量名不能为空'
    const tzMatch = inner.match(/\|tz=(.*)$/)
    if (tzMatch && !tzMatch[1]) return '时区（tz=）值不能为空'
    // Reserved time words (review C1): mirror the Go kind gate exactly
    // (pkg/trollsift parseFieldSpec) — the reserved word must be used as a
    // TIME field with an LDML format. Bare ({submit_time}) and typed
    // non-time ({submit_time:s}, {time:3d}) uses are rejected here AND by the
    // agent's reservedTimeMisuse gate; letting the UI create them would mean
    // the failure only surfaces at upload time.
    const reservedError = reservedTimeKindError(inner)
    if (reservedError) return reservedError
  }

  return null
}

/**
 * Mirror of the Go kind detection for the reserved time words
 * (pkg/trollsift/field.go parseFieldSpec, review C1 — kept line-by-line
 * comparable): strip |tz=..., then spec is
 *   d / Nd / 0Nd  → int      (rejected for reserved words)
 *   "" / s / Ns   → string   (rejected; "" is the bare form)
 *   anything else → time     (the only accepted kind)
 * Returns an error message for a reserved word used as a non-time field,
 * or null when the field is fine (not a reserved word, or time-typed).
 */
function reservedTimeKindError(inner: string): string | null {
  const colonIdx = inner.indexOf(':')
  const name = colonIdx === -1 ? inner : inner.slice(0, colonIdx)
  if (name !== 'submit_time' && name !== 'time') return null

  if (colonIdx === -1) {
    return `保留时间字段必须带 LDML 时间格式，例如 {${name}:yyyy/MM/dd}（裸 {${name}} 无法合成，会被 agent 拒绝）`
  }

  let spec = inner.slice(colonIdx + 1)
  const pipeIdx = spec.indexOf('|tz=')
  if (pipeIdx !== -1) spec = spec.slice(0, pipeIdx)

  const isInt = /^d$/.test(spec) || /^0\d+d$/.test(spec) || /^\d+d$/.test(spec)
  const isStr = spec === '' || spec === 's' || /^\d+s$/.test(spec)
  if (isInt || isStr) {
    const shown = spec === '' ? name : `${name}:${spec}`
    return `保留时间字段必须带 LDML 时间格式，例如 {${name}:yyyy/MM/dd}；非时间类型（{${shown}}）会被 agent 拒绝（IC-BUG-50）`
  }
  return null
}

/**
 * Extract field names from a trollsift path_pattern string.
 * Returns the fieldname portion from `{fieldname}` and `{fieldname:type}` tokens.
 *
 * @param pathPattern - The path_pattern string from Step 2.
 * @returns Array of field names found in the pattern.
 */
export function extractDynamicFields(pathPattern: string): string[] {
  const matches = pathPattern.match(/\{([^}]+)\}/g)
  if (!matches) return []
  return matches.map((match) => {
    const inner = match.slice(1, -1)
    const colonIdx = inner.indexOf(':')
    return colonIdx !== -1 ? inner.slice(0, colonIdx) : inner
  })
}
