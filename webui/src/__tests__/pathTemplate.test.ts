import { describe, it, expect } from 'vitest'
import {
  renderPathPreview,
  validatePathTemplate,
  extractDynamicFields,
  SYSTEM_TEMPLATE_VARIABLES,
} from '../utils/pathTemplate'

describe('SYSTEM_TEMPLATE_VARIABLES', () => {
  it('should export 4 system variables', () => {
    expect(SYSTEM_TEMPLATE_VARIABLES).toHaveLength(4)
    const keys = SYSTEM_TEMPLATE_VARIABLES.map((v) => v.key)
    expect(keys).toContain('{agent_name}')
    expect(keys).toContain('{agent_id}')
    expect(keys).toContain('{filename}')
    expect(keys).toContain('{ext}')
  })
})

describe('renderPathPreview', () => {
  // IC-BUG-16: the object key never starts with "/", so a preview that shows
  // one misrepresents what actually lands in MinIO.
  it('strips the leading slash so the preview matches the real object key', () => {
    expect(renderPathPreview('/{agent_name}/{filename}')).toBe('my-agent/data.csv')
    expect(renderPathPreview('{agent_name}/{filename}')).toBe('my-agent/data.csv')
    // Repeated separators too: the Go side uses TrimLeft, this must match.
    expect(renderPathPreview('//{agent_name}/{filename}')).toBe('my-agent/data.csv')
  })

  // R-1: LDML time field + system variables
  it('R-1: replaces LDML time field and system variables', () => {
    const preview = renderPathPreview('/{agent_name}/{time:yyyy/MM/dd}/{filename}')
    const now = new Date()
    expect(preview).toContain('my-agent')
    expect(preview).toContain('data.csv')
    // LDML-rendered date fragment: YYYY/MM/DD
    expect(preview).toMatch(/\/\d{4}\/\d{2}\/\d{2}\//)
    expect(preview).toContain(String(now.getUTCFullYear()))
  })

  // R-2: dynamic fields shown as «name»
  it('R-2: renders dynamic fields from path_pattern as «name» placeholders', () => {
    const preview = renderPathPreview('/{agent_name}/{sensor_id}/{filename}', ['sensor_id'])
    expect(preview).toContain('my-agent')
    expect(preview).toContain('\u00ABsensor_id\u00BB')
    expect(preview).toContain('data.csv')
  })

  // R-3: static path unchanged apart from the leading-slash normalisation
  it('R-3: returns static path unchanged when no variables are used', () => {
    expect(renderPathPreview('static/path')).toBe('static/path')
    // The leading "/" is dropped: it is not part of the object key.
    expect(renderPathPreview('/static/path')).toBe('static/path')
  })

  // R-4: empty string
  it('R-4: returns empty string for empty input', () => {
    const preview = renderPathPreview('')
    expect(preview).toBe('')
  })

  // R-5: LDML with hour segment
  it('R-5: renders LDML time field with hour correctly', () => {
    const preview = renderPathPreview('/{agent_name}/{time:yyyy/MM/dd/HH}/{filename}')
    // Pattern: /YYYY/MM/DD/HH/ fragment somewhere in the result
    expect(preview).toMatch(/\/\d{4}\/\d{2}\/\d{2}\/\d{2}\//)
  })

  it('leaves unknown variables unchanged when not in dynamicFields', () => {
    const preview = renderPathPreview('/{custom_var}/test')
    expect(preview).toContain('{custom_var}')
  })

  it('strips |tz= suffix and still renders LDML', () => {
    const preview = renderPathPreview('/{time:yyyy/MM|tz=Asia/Shanghai}')
    expect(preview).toMatch(/^\d{4}\/\d{2}$/)
  })
})

describe('validatePathTemplate', () => {
  // V-1: empty string
  it('V-1: rejects empty string', () => {
    expect(validatePathTemplate('')).toBeTruthy()
  })

  // V-2: a leading "/" is optional — it is stripped before the object key is
  // built, so requiring it only forced users to type a character that never
  // reaches MinIO.
  it('V-2: accepts templates with or without a leading /', () => {
    expect(validatePathTemplate('{year}/{filename}')).toBeNull()
    expect(validatePathTemplate('/{year}/{filename}')).toBeNull()
  })

  // V-3: double slashes
  it('V-3: rejects templates with double slashes', () => {
    expect(validatePathTemplate('//year')).toBeTruthy()
  })

  // V-4: unbalanced braces
  it('V-4: rejects unbalanced braces', () => {
    expect(validatePathTemplate('/{agent_name/{filename}')).toBeTruthy()
  })

  // V-5: empty field name
  it('V-5: rejects empty field name ({})', () => {
    expect(validatePathTemplate('/{}/{filename}')).toBeTruthy()
  })

  // V-6: tz= with empty value
  it('V-6: rejects |tz= with empty value', () => {
    expect(validatePathTemplate('/{t:yyyy|tz=}/{filename}')).toBeTruthy()
  })

  // V-7: valid trollsift template with LDML
  it('V-7: accepts valid template with LDML time field', () => {
    expect(validatePathTemplate('/{agent_name}/{time:yyyy/MM/dd}/{filename}')).toBeNull()
  })

  // V-8: custom fields are now valid (no whitelist)
  it('V-8: accepts custom field names without error', () => {
    expect(validatePathTemplate('/{sensor_id}/{device}/{filename}')).toBeNull()
  })

  // V-9: previously-rejected agent_name is now valid
  it('V-9: accepts {agent_name} and unknown fields (no whitelist check)', () => {
    expect(validatePathTemplate('/{agent_name}/{unknown_var}')).toBeNull()
  })

  it('accepts valid tz= with non-empty value', () => {
    expect(validatePathTemplate('/{t:yyyy/MM/dd|tz=Asia/Shanghai}/{filename}')).toBeNull()
  })
})

describe('extractDynamicFields', () => {
  // E-1: mixed fields
  it('E-1: extracts field names from trollsift pattern', () => {
    const fields = extractDynamicFields('{sensor_id}/{date:yyyy/MM/dd}/{filename}')
    expect(fields).toEqual(['sensor_id', 'date', 'filename'])
  })

  // E-2: no braces
  it('E-2: returns empty array when no {} present', () => {
    expect(extractDynamicFields('*.csv')).toEqual([])
  })

  it('handles simple field names', () => {
    expect(extractDynamicFields('{device}/{seq:05d}.csv')).toEqual(['device', 'seq'])
  })
})
