import { describe, it, expect } from 'vitest'
import {
  renderPathPreview,
  validatePathTemplate,
  extractDynamicFields,
  SYSTEM_TEMPLATE_VARIABLES,
} from '../utils/pathTemplate'

describe('SYSTEM_TEMPLATE_VARIABLES', () => {
  it('should export the system variables (IC-BUG-50: incl. submit_time)', () => {
    expect(SYSTEM_TEMPLATE_VARIABLES).toHaveLength(6)
    const keys = SYSTEM_TEMPLATE_VARIABLES.map((v) => v.key)
    expect(keys).toContain('{agent_name}')
    expect(keys).toContain('{agent_id}')
    expect(keys).toContain('{filename}')
    expect(keys).toContain('{ext}')
  })

  // IC-BUG-50: submit_time is the declared reserved word for the moment the
  // file was submitted for upload; the deprecated {time} alias is listed so
  // legacy rules are discoverable in the UI hints. Review P1-B: the list shows
  // the LDML form — a bare reserved time field ({submit_time}/{time} without a
  // format) can never compose, so the UI must not advertise it.
  it('declares submit_time and the deprecated time alias in LDML form', () => {
    const keys = SYSTEM_TEMPLATE_VARIABLES.map((v) => v.key)
    expect(keys).toContain('{submit_time:yyyy/MM/dd}')
    expect(keys).toContain('{time:yyyy/MM/dd}')
    // The bare form must not be advertised anywhere in the list.
    expect(keys).not.toContain('{submit_time}')
    expect(keys).not.toContain('{time}')
    const submitTime = SYSTEM_TEMPLATE_VARIABLES.find((v) => v.key.startsWith('{submit_time'))
    expect(submitTime?.desc).toContain('上传')
    const legacyTime = SYSTEM_TEMPLATE_VARIABLES.find((v) => v.key.startsWith('{time'))
    expect(legacyTime?.desc).toContain('deprecated')
  })
})

describe('renderPathPreview: submit_time (IC-BUG-50 / D-034)', () => {
  it('renders the new default template with LDML', () => {
    const preview = renderPathPreview('/{agent_name}/{submit_time:yyyy/MM/dd}/{filename}')
    expect(preview).toMatch(/^my-agent\/\d{4}\/\d{2}\/\d{2}\/data\.csv$/)
  })

  // Review P1-B: the bare form is forbidden — it cannot compose at upload
  // time, and the deprecation hint must never leak into the previewed key.
  it('leaves the bare reserved word unchanged (no fake value, no hint text in the key)', () => {
    expect(renderPathPreview('{submit_time}')).toBe('{submit_time}')
    expect(renderPathPreview('{time}')).toBe('{time}')
    expect(renderPathPreview('{time}')).not.toContain('deprecated')
  })

  // Review P1-B: the validator rejects the bare form with a readable message.
  it('validatePathTemplate rejects the bare reserved time words', () => {
    expect(validatePathTemplate('{submit_time}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{time}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{submit_time:yyyy/MM/dd}/{filename}')).toBeNull()
    // time_zone is an ordinary field name, not the reserved word.
    expect(validatePathTemplate('{time_zone}/{filename}')).toBeNull()
  })

  // Review C1: the validator must mirror the Go kind gate, not just the bare
  // form — a typed NON-time reference ({submit_time:s}, {time:3d}) composes
  // when the value was parsed as a string and is refused by the agent.
  it('validatePathTemplate rejects typed non-time reserved words (kind gate parity)', () => {
    expect(validatePathTemplate('{submit_time:s}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{time:s}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{submit_time:3s}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{time:3d}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{time:d}/{filename}')).toContain('LDML')
    expect(validatePathTemplate('{time:05d}/{filename}')).toContain('LDML') // zero-pad int variant
    // The supported forms stay valid.
    expect(validatePathTemplate('{submit_time:yyyy/MM/dd}/{filename}')).toBeNull()
    expect(validatePathTemplate('{time:yyyy|tz=Asia/Shanghai}/{filename}')).toBeNull()
    expect(validatePathTemplate('{time:HH:mm:ss|tz=Asia/Shanghai}/{filename}')).toBeNull()
    // time_zone with the same specs is an ordinary field: untouched.
    expect(validatePathTemplate('{time_zone:s}/{filename}')).toBeNull()
    expect(validatePathTemplate('{time_zone:3d}/{filename}')).toBeNull()
  })

  it('does not treat {time_zone} as a time field', () => {
    // time_zone without LDML is not a reserved word: it stays unchanged
    // (compose-time field from path_pattern), exactly like unknown variables.
    expect(renderPathPreview('{time_zone}', ['time_zone'])).toBe('\u00ABtime_zone\u00BB')
  })
})

describe('renderPathPreview: parsed fields win over the current-time rendering (review P2-C)', () => {
  it('shows «name» for a dynamic field referenced with LDML — new name', () => {
    // path_pattern parses submit_time (e.g. the data date): the local preview
    // must not show the current time, which is what the upload would NOT use.
    expect(renderPathPreview('{submit_time:yyyy/MM}/{filename}', ['submit_time']))
      .toBe('\u00ABsubmit_time\u00BB/data.csv')
  })

  it('shows «name» for a dynamic field referenced with LDML — legacy name', () => {
    expect(renderPathPreview('{time:yyyy/MM}/{filename}', ['time']))
      .toBe('\u00ABtime\u00BB/data.csv')
  })

  it('still renders the current time for LDML fields that path_pattern does NOT parse', () => {
    expect(renderPathPreview('{submit_time:yyyy/MM}/{filename}'))
      .toMatch(/^\d{4}\/\d{2}\/data\.csv$/)
  })

  // Review B2: the preview must mirror the cross-alias semantics of
  // trollsift.InjectSubmitTime — when exactly ONE reserved word was parsed,
  // the missing alias composes with the SAME parsed value, so the preview
  // shows the SOURCE field's «name» for both spellings.
  it('mirrors parsed legacy time to the submit_time spelling', () => {
    expect(renderPathPreview('{submit_time:yyyy}/{filename}', ['time']))
      .toBe('\u00ABtime\u00BB/data.csv')
  })

  it('mirrors parsed submit_time to the legacy time spelling', () => {
    expect(renderPathPreview('{time:yyyy}/{filename}', ['submit_time']))
      .toBe('\u00ABsubmit_time\u00BB/data.csv')
  })

  it('keeps each spelling on its own parsed value when both were parsed', () => {
    expect(renderPathPreview('{time:yyyy}/{submit_time:yyyy}/{filename}', ['time', 'submit_time']))
      .toBe('\u00ABtime\u00BB/\u00ABsubmit_time\u00BB/data.csv')
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
