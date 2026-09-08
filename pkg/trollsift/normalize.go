package trollsift

import "strings"

// NormalizeTemplate canonicalises a dest_path_template so that every consumer
// derives the same object key from it.
//
// The rule is a single one: an object key never starts with "/". The agent has
// always stripped the leading slash from the *composed path* before uploading,
// but the Control Plane used to reverse-parse the *raw template* against that
// already-stripped key, so any template written as "/{year}/{filename}" failed
// to match and path-variable tagging silently did nothing (IC-BUG-16). The Web
// UI preview had the same split, showing "/my-agent/..." for a key that is
// really "my-agent/...".
//
// Normalising the template — rather than making the agent stop stripping the
// path — is deliberate: the reverse direction would change every object key
// ever written and require re-laying out existing data.
//
// Callers should normalise the template once, then use the result for both
// Compose and Parse. See docs/design/contracts.md V-3.
func NormalizeTemplate(template string) string {
	return strings.TrimPrefix(template, "/")
}

// NormalizeObjectKey applies the same canonical form to a composed path, so a
// key and the template it came from can be compared without either side
// carrying a stray leading separator.
func NormalizeObjectKey(key string) string {
	return strings.TrimPrefix(key, "/")
}
