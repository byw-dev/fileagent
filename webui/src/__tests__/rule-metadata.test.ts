import { describe, it, expect } from 'vitest'
import {
  toRuleMetadata,
  metadataToFormFields,
  PATH_VAR_RE,
  templateVarName,
  pathTemplateVars,
} from '../pages/Agents/ruleMetadata'

describe('RuleForm metadata conversion', () => {
  it('toRuleMetadata builds the metadata object, trimming keys/values and dropping empties', () => {
    const meta = toRuleMetadata({
      file_type: '  pressure  ',
      static_tags: [
        { key: ' vendor ', value: ' omron ' }, // key + value both trimmed
        { key: 'bad', value: '   ' }, // dropped: whitespace-only value
        { key: '  ', value: 'x' }, // dropped: whitespace-only key
      ],
      path_tag_map: [{ key: ' site ', template: ' {site} ' }],
    })
    expect(meta).toEqual({
      file_type: 'pressure',
      static_tags: { vendor: 'omron' },
      path_tag_map: { site: '{site}' },
    })
  })

  it('toRuleMetadata omits empty sections entirely', () => {
    expect(toRuleMetadata({ file_type: '', static_tags: [], path_tag_map: [] })).toEqual({})
  })

  it('metadataToFormFields converts maps back into row arrays', () => {
    const fields = metadataToFormFields({
      file_type: 'pressure',
      static_tags: { vendor: 'omron' },
      path_tag_map: { site: '{site}' },
    })
    expect(fields.file_type).toBe('pressure')
    expect(fields.static_tags).toEqual([{ key: 'vendor', value: 'omron' }])
    expect(fields.path_tag_map).toEqual([{ key: 'site', template: '{site}' }])
  })

  it('metadataToFormFields sorts rows by key for deterministic display', () => {
    const fields = metadataToFormFields({
      static_tags: { vendor: 'omron', line: 'A', bay: '3' },
      path_tag_map: { site: '{site}', level: '{level}' },
    })
    expect(fields.static_tags.map((r) => r.key)).toEqual(['bay', 'line', 'vendor'])
    expect(fields.path_tag_map.map((r) => r.key)).toEqual(['level', 'site'])
  })

  it('metadataToFormFields yields empty defaults for undefined metadata', () => {
    const fields = metadataToFormFields(undefined)
    expect(fields.file_type).toBe('')
    expect(fields.static_tags).toEqual([])
    expect(fields.path_tag_map).toEqual([])
  })

  it('PATH_VAR_RE accepts a single {var} / {var:fmt} (incl. inner whitespace) and rejects other forms', () => {
    expect(PATH_VAR_RE.test('{site}')).toBe(true)
    expect(PATH_VAR_RE.test('{time:yyyy/MM/dd}')).toBe(true)
    expect(PATH_VAR_RE.test('{ site }')).toBe(true) // inner whitespace, like the backend
    expect(PATH_VAR_RE.test('site')).toBe(false) // no braces
    expect(PATH_VAR_RE.test('{site}/{x}')).toBe(false) // more than one variable
    expect(PATH_VAR_RE.test('prefix-{site}')).toBe(false) // extra text
    expect(PATH_VAR_RE.test('{}')).toBe(false) // empty
  })

  it('templateVarName extracts the variable name (trimming, dropping format)', () => {
    expect(templateVarName('{site}')).toBe('site')
    expect(templateVarName(' { site } ')).toBe('site') // outer + inner whitespace
    expect(templateVarName('{time:yyyy/MM/dd}')).toBe('time')
    expect(templateVarName('not-a-var')).toBe('')
  })

  it('pathTemplateVars collects variable names from a path template', () => {
    const vars = pathTemplateVars('/{agent_name}/{site}/{time:yyyy/MM/dd}/{filename}')
    expect([...vars].sort()).toEqual(['agent_name', 'filename', 'site', 'time'])
  })

  it('round-trips metadata through form fields and back', () => {
    const original = {
      file_type: 'vibration',
      static_tags: { vendor: 'omron', line: 'A' },
      path_tag_map: { site: '{site}' },
    }
    const fields = metadataToFormFields(original)
    expect(toRuleMetadata(fields)).toEqual(original)
  })
})
