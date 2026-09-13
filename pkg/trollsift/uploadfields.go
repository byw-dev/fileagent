package trollsift

import (
	"regexp"
	"time"
)

// Reserved upload-path variables (IC-BUG-50 / D-034).
//
// submit_time is the declared reserved word for the moment the file was
// submitted for upload (the submitFile instant, UTC). time is its deprecated
// alias, kept rendering legacy rules created through the Web UI's old default
// template /{agent_name}/{time:yyyy/MM/dd}/{filename}.
const (
	SubmitTimeField     = "submit_time"
	DeprecatedTimeField = "time"
)

// InjectSubmitTime injects the reserved upload-time variables into fields.
//
// time and submit_time are aliases of one another (D-034, review P1-A):
// when exactly one of them was parsed out of path_pattern, the parsed value is
// MIRRORED to the missing alias — otherwise migrating a template from {time}
// to {submit_time} (what the deprecation hint advises) would silently change
// the object key from the data date to the upload instant. When both were
// parsed, each keeps its own value: Compose reads only the fields the template
// references, and force-mirroring two independent parse results would
// fabricate a value neither asked for. When neither was parsed, both get the
// submit instant. The injection is unconditional-if-absent rather than gated
// on a template substring check: unreferenced fields do not participate in
// Compose, so this cannot misfire on field names that merely share a prefix
// ({time_zone}).
func InjectSubmitTime(fields map[string]Value, at time.Time) map[string]Value {
	if fields == nil {
		fields = make(map[string]Value)
	}
	parsedSubmit, hasSubmit := fields[SubmitTimeField]
	parsedLegacy, hasLegacy := fields[DeprecatedTimeField]
	switch {
	case hasSubmit && hasLegacy:
		// Both parsed: keep each as-is (see doc comment).
	case hasSubmit:
		fields[DeprecatedTimeField] = parsedSubmit
	case hasLegacy:
		fields[SubmitTimeField] = parsedLegacy
	default:
		fields[SubmitTimeField] = T(at)
		// Deprecated alias (D-034): legacy {time} templates keep rendering with
		// identical semantics. Not removed; no removal date.
		fields[DeprecatedTimeField] = T(at)
	}
	return fields
}

// deprecatedTimeRe matches the deprecated reserved word {time} / {time:LDML}
// but not longer field names sharing the prefix ({time_zone}, {times}).
var deprecatedTimeRe = regexp.MustCompile(`\{time[:}]`)

// UsesDeprecatedTimeField reports whether a dest_path_template references the
// deprecated {time} reserved word, so rule creation/update can surface a
// readable deprecation hint (D-034).
func UsesDeprecatedTimeField(template string) bool {
	return deprecatedTimeRe.MatchString(template)
}

// bareReservedTimeRe matches the reserved time words in their BARE form —
// {submit_time} or {time} without an LDML format (review P1-B).
var bareReservedTimeRe = regexp.MustCompile(`\{submit_time\}|\{time\}`)

// UsesBareReservedTimeField reports whether a dest_path_template references a
// reserved time word bare (no LDML). Such templates cannot compose: Go types a
// format-less field as string while the injected value is a time, so every
// upload would fail with "expects a string value". CP creation/update warns
// on it; the webui validator rejects it outright.
func UsesBareReservedTimeField(template string) bool {
	return bareReservedTimeRe.MatchString(template)
}

// ValidateReservedTimeUse rejects the reserved time words when they are used
// as anything OTHER than a time field with an LDML format (review B1). The
// check is bidirectional: dest_path_template must not reference the reserved
// word bare or typed as string/int ({submit_time}, {submit_time:s},
// {time:3d}…), and path_pattern must not declare it as a non-time field
// ({submit_time:s} parsing an arbitrary substring silently repurposes the
// reserved word as an ordinary path segment). Bare reserved fields cannot
// compose at all; non-time typed references either fail on the injected value
// or, worse, compose with a parsed string and break the contract's promise
// that submit_time is the file's submit-for-upload instant.
//
// Returns "" when the pattern is acceptable; otherwise a readable reason for
// the agent's IC-BUG-21 refusal and the CP's deprecation warnings. Syntax
// errors return "" — they are reported by New at the caller's own site.
func ValidateReservedTimeUse(pattern string) string {
	p, err := New(pattern)
	if err != nil {
		return ""
	}
	for _, seg := range p.segments {
		if !seg.isField {
			continue
		}
		fs := seg.field
		if fs.name != SubmitTimeField && fs.name != DeprecatedTimeField {
			continue
		}
		if fs.kind != kindTime {
			return "reserved time word \"" + fs.name + "\" must be used as a time field with an LDML format, e.g. {" +
				fs.name + ":yyyy/MM/dd}; bare (" + "{" + fs.name + "}) and non-time typed (" + "{" + fs.name + ":s}) uses are rejected (IC-BUG-50 / D-034)"
		}
	}
	return ""
}