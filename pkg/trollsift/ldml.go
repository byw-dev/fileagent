package trollsift

import (
	"regexp"
	"strings"
	"time"
)

type ldmlEntry struct {
	sym    string
	reStr  string // regex fragment (no capturing group)
	layout string // Go time layout token
	globW  int    // number of '?' chars in Globify output
}

// ldmlTable defines supported LDML symbols, longest-first to prevent partial matches.
var ldmlTable = []ldmlEntry{
	{"yyyy", `\d{4}`, "2006", 4},
	{"MM", `\d{2}`, "01", 2},
	{"dd", `\d{2}`, "02", 2},
	{"HH", `\d{2}`, "15", 2},
	{"mm", `\d{2}`, "04", 2},
	{"ss", `\d{2}`, "05", 2},
	{"yy", `\d{2}`, "06", 2},
}

// ldmlToRegexStr converts an LDML format string to a regex fragment (no outer group).
// Non-LDML characters are regex-escaped and preserved literally.
func ldmlToRegexStr(ldml string) string {
	var sb strings.Builder
	i := 0
	for i < len(ldml) {
		if sym, ok := matchLDML(ldml, i); ok {
			sb.WriteString(sym.reStr)
			i += len(sym.sym)
		} else {
			sb.WriteString(regexp.QuoteMeta(string(ldml[i])))
			i++
		}
	}
	return sb.String()
}

// ldmlToLayout converts an LDML format string to a Go time.Parse layout string.
func ldmlToLayout(ldml string) string {
	var sb strings.Builder
	i := 0
	for i < len(ldml) {
		if sym, ok := matchLDML(ldml, i); ok {
			sb.WriteString(sym.layout)
			i += len(sym.sym)
		} else {
			sb.WriteByte(ldml[i])
			i++
		}
	}
	return sb.String()
}

// ldmlToGlobStr converts an LDML format string to a glob fragment.
// Each LDML symbol becomes that many '?' characters; other characters are preserved.
func ldmlToGlobStr(ldml string) string {
	var sb strings.Builder
	i := 0
	for i < len(ldml) {
		if sym, ok := matchLDML(ldml, i); ok {
			for j := 0; j < sym.globW; j++ {
				sb.WriteByte('?')
			}
			i += len(sym.sym)
		} else {
			sb.WriteByte(ldml[i])
			i++
		}
	}
	return sb.String()
}

// parseTimeField parses s using the LDML format in the given location.
// The returned time is always in UTC.
func parseTimeField(ldml, s string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	layout := ldmlToLayout(ldml)
	t, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// formatTimeField formats t (UTC) using the LDML format in the given location.
// The time is converted to loc before formatting.
func formatTimeField(t time.Time, ldml string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	layout := ldmlToLayout(ldml)
	return t.In(loc).Format(layout)
}

// matchLDML checks whether any LDML symbol in ldmlTable starts at position i of s.
func matchLDML(s string, i int) (ldmlEntry, bool) {
	rest := s[i:]
	for _, e := range ldmlTable {
		if strings.HasPrefix(rest, e.sym) {
			return e, true
		}
	}
	return ldmlEntry{}, false
}
