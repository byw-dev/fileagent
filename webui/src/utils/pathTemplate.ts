/**
 * Supported template variables for upload path configuration.
 * These are resolved at upload time by the Agent.
 */
export const PATH_TEMPLATE_VARIABLES: Record<string, string> = {
  '{year}': '当前年份（4位），例如 2025',
  '{month}': '当前月份（2位），例如 04',
  '{day}': '当前日期（2位），例如 15',
  '{hour}': '当前小时（2位，24小时制），例如 09',
  '{agent_id}': '采集器 UUID',
  '{agent_name}': '采集器名称',
  '{file_type}': '文件类型名称',
  '{filename}': '原始文件名（含扩展名）',
  '{ext}': '文件扩展名（不含点号），例如 csv',
}

/**
 * Render a preview of a path template by substituting variables with example values.
 * @param template - Path template string, e.g. `/{year}/{month}/{agent_name}/{filename}`.
 * @returns Preview string with example substitutions applied.
 */
export function renderPathPreview(template: string): string {
  const now = new Date()
  const pad = (n: number, len = 2) => String(n).padStart(len, '0')

  const substitutions: Record<string, string> = {
    '{year}': String(now.getUTCFullYear()),
    '{month}': pad(now.getUTCMonth() + 1),
    '{day}': pad(now.getUTCDate()),
    '{hour}': pad(now.getUTCHours()),
    '{agent_id}': 'agent-uuid-example',
    '{agent_name}': 'my-agent',
    '{file_type}': 'var_hourly',
    '{filename}': 'data_20250415.csv',
    '{ext}': 'csv',
  }

  return Object.entries(substitutions).reduce(
    (result, [key, value]) => result.replaceAll(key, value),
    template
  )
}

/**
 * Validate that a path template contains only known variables and no double slashes.
 * @param template - The template string to validate.
 * @returns An error message string, or null if the template is valid.
 */
export function validatePathTemplate(template: string): string | null {
  if (!template) return '路径模板不能为空'
  if (!template.startsWith('/')) return '路径模板必须以 / 开头'
  if (template.includes('//')) return '路径模板不能包含连续的 //'

  const variablePattern = /\{[^}]+\}/g
  const used = template.match(variablePattern) ?? []
  const unknown = used.filter((v) => !(v in PATH_TEMPLATE_VARIABLES))
  if (unknown.length > 0) {
    return `未知的模板变量：${unknown.join(', ')}`
  }

  return null
}
