package trollsift

import (
	"fmt"
	"regexp"
	"strings"
)

// buildParseRegex constructs the un-anchored regex string and the ordered list
// of field specs (matching the capture-group order) for use in Parse.
func buildParseRegex(segs []segment) (string, []fieldSpec, error) {
	var sb strings.Builder
	var fields []fieldSpec

	for _, seg := range segs {
		if !seg.isField {
			sb.WriteString(regexp.QuoteMeta(seg.literal))
			continue
		}

		fs := seg.field
		fields = append(fields, fs)

		switch fs.kind {
		case kindStr:
			if fs.width > 0 {
				sb.WriteString(fmt.Sprintf(`(.{%d})`, fs.width))
			} else {
				// Non-greedy; the $ anchor expands it as needed.
				sb.WriteString(`(.+?)`)
			}
		case kindInt:
			if fs.width > 0 {
				sb.WriteString(fmt.Sprintf(`(\d{%d})`, fs.width))
			} else {
				sb.WriteString(`(\d+)`)
			}
		case kindTime:
			sb.WriteString(`(` + ldmlToRegexStr(fs.ldml) + `)`)
		default:
			return "", nil, fmt.Errorf("trollsift: unsupported field kind %d for field %q", fs.kind, fs.name)
		}
	}

	return sb.String(), fields, nil
}
