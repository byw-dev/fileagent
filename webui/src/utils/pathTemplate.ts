/**
 * System template variables that are always available for upload path configuration.
 * These are injected by the Agent via InjectContext and buildStoragePath.
 */
export const SYSTEM_TEMPLATE_VARIABLES: ReadonlyArray<{ key: string; desc: string }> = [
  { key: '{agent_name}', desc: 'Agent 名称，例如 prod-sensor-01' },
  { key: '{agent_id}',   desc: 'Agent UUID，例如 a1b2c3...' },
  { key: '{filename}',   desc: '原始文件名（含扩展名），例如 data.csv' },
  { key: '{ext}',        desc: '文件扩展名（不含点），例如 csv' },
]

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
 *  1. `{fieldname:LDML}` or `{fieldname:LDML|tz=...}` — formatted with UTC current time.
 *  2. System variables (`{agent_name}`, `{agent_id}`, `{filename}`, `{ext}`) — fixed examples.
 *  3. Fields present in `dynamicFields` — shown as `«fieldname»` (runtime placeholder).
 *  4. All other variables — left unchanged (valid syntax, resolved at runtime).
 *
 * @param template - Path template string, e.g. `/{agent_name}/{time:yyyy/MM/dd}/{filename}`.
 * @param dynamicFields - Field names extracted from path_pattern (shown as «name» placeholders).
 * @returns Preview string with example substitutions applied.
 */
export function renderPathPreview(template: string, dynamicFields?: string[]): string {
  if (!template) return template

  const now = new Date()

  const systemSubs: Record<string, string> = {
    '{agent_name}': 'my-agent',
    '{agent_id}':   'a1b2c3d4-e5f6-7890-abcd-ef1234567890',
    '{filename}':   'data.csv',
    '{ext}':        'csv',
  }

  const dynamicSet = new Set(dynamicFields ?? [])

  return template.replace(/\{([^}]+)\}/g, (match, inner: string) => {
    const colonIdx = inner.indexOf(':')
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
    if (dynamicSet.has(inner)) {
      return `\u00AB${inner}\u00BB`
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
  if (!template.startsWith('/')) return '路径模板必须以 / 开头'
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
