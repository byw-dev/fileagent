import type { RuleMetadata } from '../../services/agents'

/** One static-tag row in the rule form (a fixed key→value applied to every file). */
export interface StaticTagRow {
  key: string
  value: string
}

/** One path-tag-map row: a tag key populated from a path-template variable. */
export interface PathTagRow {
  key: string
  template: string
}

/** The metadata step's editable fields (maps are edited as row arrays). */
export interface RuleMetadataFields {
  file_type?: string
  static_tags?: StaticTagRow[]
  path_tag_map?: PathTagRow[]
}

/** Convert the metadata step's row arrays into the RuleMetadata object stored on
 * the rule, dropping incomplete rows and omitting empty sections. */
export function toRuleMetadata(values: RuleMetadataFields): RuleMetadata {
  const meta: RuleMetadata = {}
  if (values.file_type?.trim()) meta.file_type = values.file_type.trim()

  const staticTags: Record<string, string> = {}
  for (const row of values.static_tags ?? []) {
    if (row?.key && row.value?.trim()) staticTags[row.key] = row.value.trim()
  }
  if (Object.keys(staticTags).length > 0) meta.static_tags = staticTags

  const pathMap: Record<string, string> = {}
  for (const row of values.path_tag_map ?? []) {
    if (row?.key && row.template?.trim()) pathMap[row.key] = row.template.trim()
  }
  if (Object.keys(pathMap).length > 0) meta.path_tag_map = pathMap

  return meta
}

/** Convert a stored RuleMetadata object back into the form's row arrays. */
export function metadataToFormFields(meta?: RuleMetadata): Required<RuleMetadataFields> {
  return {
    file_type: meta?.file_type ?? '',
    static_tags: Object.entries(meta?.static_tags ?? {}).map(([key, value]) => ({ key, value })),
    path_tag_map: Object.entries(meta?.path_tag_map ?? {}).map(([key, template]) => ({ key, template })),
  }
}
