package trollsift

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type fieldKind int

const (
	kindStr  fieldKind = iota
	kindInt
	kindTime
)

// fieldSpec holds the parsed format specification for a single pattern field.
type fieldSpec struct {
	name    string
	kind    fieldKind
	width   int            // 0 = variable; >0 = fixed width
	zeroPad bool           // for kindInt: zero-pad on Compose
	ldml    string         // for kindTime: LDML format string
	tz      *time.Location // for kindTime: output/input timezone (nil = UTC)
	rawTZ   string         // original IANA string for round-trip in Compose partial
}

var (
	reIntVar     = regexp.MustCompile(`^d$`)
	reIntZeroPad = regexp.MustCompile(`^0(\d+)d$`)
	reIntFixed   = regexp.MustCompile(`^(\d+)d$`)
	reStrFixed   = regexp.MustCompile(`^(\d+)s$`)
)

// parseFieldSpec parses the raw spec string (part after ':' in a field token)
// together with its field name. Returns a populated fieldSpec or a syntax error.
func parseFieldSpec(name, spec string) (fieldSpec, error) {
	if name == "" {
		return fieldSpec{}, fmt.Errorf("trollsift: empty field name")
	}

	// Extract optional |tz= suffix before type detection.
	var tzStr string
	if idx := strings.Index(spec, "|tz="); idx >= 0 {
		tzStr = spec[idx+4:]
		spec = spec[:idx]
		if tzStr == "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= value is empty for field %q", name)
		}
	}

	var fs fieldSpec
	fs.name = name

	// --- integer: d, Nd, 0Nd ---
	if reIntVar.MatchString(spec) {
		if tzStr != "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= is not valid for integer field %q", name)
		}
		fs.kind = kindInt
		return fs, nil
	}
	if m := reIntZeroPad.FindStringSubmatch(spec); m != nil {
		if tzStr != "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= is not valid for integer field %q", name)
		}
		n, _ := strconv.Atoi(m[1])
		fs.kind = kindInt
		fs.width = n
		fs.zeroPad = true
		return fs, nil
	}
	if m := reIntFixed.FindStringSubmatch(spec); m != nil {
		if tzStr != "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= is not valid for integer field %q", name)
		}
		n, _ := strconv.Atoi(m[1])
		fs.kind = kindInt
		fs.width = n
		return fs, nil
	}

	// --- string: (empty), s, Ns ---
	if spec == "" || spec == "s" {
		if tzStr != "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= is not valid for string field %q", name)
		}
		fs.kind = kindStr
		return fs, nil
	}
	if m := reStrFixed.FindStringSubmatch(spec); m != nil {
		if tzStr != "" {
			return fieldSpec{}, fmt.Errorf("trollsift: |tz= is not valid for string field %q", name)
		}
		n, _ := strconv.Atoi(m[1])
		fs.kind = kindStr
		fs.width = n
		return fs, nil
	}

	// --- time (LDML) ---
	if tzStr == "" {
		fs.tz = time.UTC
	} else {
		loc, err := time.LoadLocation(tzStr)
		if err != nil {
			return fieldSpec{}, fmt.Errorf("trollsift: invalid timezone %q for field %q: %w", tzStr, name, err)
		}
		fs.tz = loc
		fs.rawTZ = tzStr
	}
	fs.kind = kindTime
	fs.ldml = spec
	return fs, nil
}

// fieldSpecString reconstructs the format spec portion (the part that follows ':') of fs.
// Used to rebuild placeholder tokens in Compose with allowPartial=true.
func fieldSpecString(fs fieldSpec) string {
	switch fs.kind {
	case kindStr:
		if fs.width > 0 {
			return fmt.Sprintf("%ds", fs.width)
		}
		return "s"
	case kindInt:
		if fs.width > 0 {
			if fs.zeroPad {
				return fmt.Sprintf("0%dd", fs.width)
			}
			return fmt.Sprintf("%dd", fs.width)
		}
		return "d"
	case kindTime:
		if fs.rawTZ != "" {
			return fs.ldml + "|tz=" + fs.rawTZ
		}
		return fs.ldml
	}
	return ""
}
