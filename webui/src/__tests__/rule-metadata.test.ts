import { describe, it, expect } from 'vitest'
import { toRuleMetadata, metadataToFormFields } from '../pages/Agents/ruleMetadata'

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

  it('metadataToFormFields yields empty defaults for undefined metadata', () => {
    const fields = metadataToFormFields(undefined)
    expect(fields.file_type).toBe('')
    expect(fields.static_tags).toEqual([])
    expect(fields.path_tag_map).toEqual([])
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
