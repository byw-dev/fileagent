import type { RuleMetadata } from '../../services/agents'

/** A path-tag-map template must be a single trollsift variable — {var} or
 * {var:fmt}. The indexer ignores any other form, so it is validated up front. */
export const PATH_VAR_RE = /^\{[a-zA-Z_][a-zA-Z0-9_]*(:[^{}]+)?\}$/

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
    const key = row?.key?.trim()
    const value = row?.value?.trim()
    if (key && value) staticTags[key] = value
  }
  if (Object.keys(staticTags).length > 0) meta.static_tags = staticTags

  const pathMap: Record<string, string> = {}
  for (const row of values.path_tag_map ?? []) {
    const key = row?.key?.trim()
    const template = row?.template?.trim()
    if (key && template) pathMap[key] = template
  }
  if (Object.keys(pathMap).length > 0) meta.path_tag_map = pathMap

  return meta
}

/** Convert a stored RuleMetadata object back into the form's row arrays. Rows are
 * sorted by key so the display is deterministic regardless of the source object's
 * key order (e.g. Go map marshaling), avoiding noisy edit/save churn. */
export function metadataToFormFields(meta?: RuleMetadata): Required<RuleMetadataFields> {
  const byKey = <T,>(o: Record<string, T>, make: (k: string, v: T) => unknown) =>
    Object.keys(o)
      .sort()
      .map((k) => make(k, o[k]))
  return {
    file_type: meta?.file_type ?? '',
    static_tags: byKey(meta?.static_tags ?? {}, (key, value) => ({ key, value })) as StaticTagRow[],
    path_tag_map: byKey(meta?.path_tag_map ?? {}, (key, template) => ({ key, template })) as PathTagRow[],
  }
}
