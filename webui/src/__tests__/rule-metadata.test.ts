import { describe, it, expect } from 'vitest'
import {
  toRuleMetadata,
  metadataToFormFields,
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

  it('templateVarName mirrors the backend (any non-empty name, no charset whitelist)', () => {
    expect(templateVarName('{site}')).toBe('site')
    expect(templateVarName(' { site } ')).toBe('site') // outer + inner whitespace
    expect(templateVarName('{time:yyyy/MM/dd}')).toBe('time') // format dropped
    // Names the backend accepts but a [a-zA-Z_]... regex would wrongly reject:
    expect(templateVarName('{123}')).toBe('123')
    expect(templateVarName('{my-var}')).toBe('my-var')
    expect(templateVarName('{a.b}')).toBe('a.b')
    // Invalid forms:
    expect(templateVarName('site')).toBe('') // no braces
    expect(templateVarName('{site}/{x}')).toBe('') // inner braces / more than one
    expect(templateVarName('prefix-{site}')).toBe('') // extra text
    expect(templateVarName('{}')).toBe('') // empty
    expect(templateVarName('{ }')).toBe('') // whitespace-only name
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
