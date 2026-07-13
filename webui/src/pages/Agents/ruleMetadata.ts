import type { RuleMetadata } from '../../services/agents'

/** Extract the variable name from a single {var} / {var:fmt} reference, or '' if
 * it is not a valid single reference. Mirrors the backend templateVarName
 * (controlplane/internal/indexer/indexer.go): outer-trim, require a single
 * {...} with no inner braces, drop the format after the first ':', inner-trim.
 * Deliberately does NOT restrict the name to [a-zA-Z_][a-zA-Z0-9_]* — the backend
 * and path template accept any non-empty name, so the UI must not be stricter. */
export function templateVarName(ref: string): string {
  const trimmed = ref.trim()
  if (trimmed.length < 3 || trimmed[0] !== '{' || trimmed[trimmed.length - 1] !== '}') return ''
  let inner = trimmed.slice(1, -1)
  if (inner.includes('{') || inner.includes('}')) return ''
  const colon = inner.indexOf(':')
  if (colon >= 0) inner = inner.slice(0, colon)
  return inner.trim()
}

/** All variable names referenced by a path template like /{a}/{b:fmt}/{c}. */
export function pathTemplateVars(template: string): Set<string> {
  const vars = new Set<string>()
  for (const m of template.matchAll(/\{([^{}]+)\}/g)) {
    const name = m[1].split(':')[0].trim()
    if (name) vars.add(name)
  }
  return vars
}

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

  // Null-prototype maps: keys come from tenant-controlled data, so assigning a
  // key like `__proto__` must create an own property rather than touch the
  // prototype (prototype-pollution hardening). JSON serialization is unaffected.
  const staticTags: Record<string, string> = Object.create(null)
  for (const row of values.static_tags ?? []) {
    const key = row?.key?.trim()
    const value = row?.value?.trim()
    if (key && value) staticTags[key] = value
  }
  if (Object.keys(staticTags).length > 0) meta.static_tags = staticTags

  const pathMap: Record<string, string> = Object.create(null)
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
  const byKey = <R>(o: Record<string, string>, make: (key: string, value: string) => R): R[] =>
    Object.keys(o)
      .sort()
      .map((k) => make(k, o[k]))
  return {
    file_type: meta?.file_type ?? '',
    static_tags: byKey(meta?.static_tags ?? {}, (key, value) => ({ key, value })),
    path_tag_map: byKey(meta?.path_tag_map ?? {}, (key, template) => ({ key, template })),
  }
}
