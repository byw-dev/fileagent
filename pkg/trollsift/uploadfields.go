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
// Priority is parse-first (D-034): a field parsed out of path_pattern is never
// overwritten — the same rule InjectContext applies to agent_name/agent_id.
// The injection is unconditional-if-absent rather than gated on a template
// substring check: unreferenced fields do not participate in Compose, so this
// cannot misfire on field names that merely share a prefix ({time_zone}).
func InjectSubmitTime(fields map[string]Value, at time.Time) map[string]Value {
	if fields == nil {
		fields = make(map[string]Value)
	}
	if _, exists := fields[SubmitTimeField]; !exists {
		fields[SubmitTimeField] = T(at)
	}
	// Deprecated alias (D-034): legacy {time} templates keep rendering with
	// identical parse-first semantics. Not removed; no removal date.
	if _, exists := fields[DeprecatedTimeField]; !exists {
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