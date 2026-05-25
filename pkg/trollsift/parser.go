// Package trollsift provides structured path template parsing, composition, and glob conversion.
// A pattern is a string containing literal text and field placeholders of the form
// {name}, {name:fmt}, or {name:fmt|tz=IANA}.
package trollsift

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Value holds the typed value of a parsed field.
// Exactly one of IsStr / IsInt / IsTime is true.
// Raw always contains the original substring matched from the input path.
type Value struct {
	Raw    string
	Str    string
	IsStr  bool
	Int    int
	IsInt  bool
	Time   time.Time
	IsTime bool
}

// S creates a string Value.
func S(s string) Value { return Value{Str: s, IsStr: true} }

// I creates an integer Value.
func I(n int) Value { return Value{Int: n, IsInt: true} }

// T creates a time Value. The time should be in UTC; Parse stores times in UTC.
func T(t time.Time) Value { return Value{Time: t, IsTime: true} }

// segment is either a literal string or a field reference within a pattern.
type segment struct {
	isField bool
	literal string    // used when !isField
	field   fieldSpec // used when isField
}

// Parser holds the compiled representation of a trollsift pattern.
type Parser struct {
	pattern  string
	segments []segment
}

// IsTrollsiftPattern reports whether s contains at least one '{' and is therefore
// a trollsift pattern rather than a plain glob or literal string.
func IsTrollsiftPattern(s string) bool {
	return strings.ContainsRune(s, '{')
}

// New parses pattern and returns a ready-to-use Parser, or a syntax error.
func New(pattern string) (*Parser, error) {
	segs, err := parsePattern(pattern)
	if err != nil {
		return nil, err
	}
	p := &Parser{pattern: pattern, segments: segs}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// parsePattern splits pattern into literal and field segments.
func parsePattern(pattern string) ([]segment, error) {
	var segs []segment
	i := 0
	for i < len(pattern) {
		open := strings.IndexByte(pattern[i:], '{')
		close := strings.IndexByte(pattern[i:], '}')

		if open < 0 && close < 0 {
			segs = append(segs, segment{literal: pattern[i:]})
			break
		}

		// Unmatched '}' before any '{'
		if close >= 0 && (open < 0 || close < open) {
			return nil, fmt.Errorf("trollsift: unbalanced '}' in pattern %q", pattern)
		}

		// Capture literal text that precedes the '{'
		if open > 0 {
			segs = append(segs, segment{literal: pattern[i : i+open]})
		}
		i += open + 1 // advance past '{'

		// Find the closing '}'
		closeRel := strings.IndexByte(pattern[i:], '}')
		if closeRel < 0 {
			return nil, fmt.Errorf("trollsift: unbalanced '{' in pattern %q", pattern)
		}

		fieldContent := pattern[i : i+closeRel]
		i += closeRel + 1 // advance past '}'

		// Split into name and optional format spec
		var name, spec string
		if idx := strings.IndexByte(fieldContent, ':'); idx >= 0 {
			name = fieldContent[:idx]
			spec = fieldContent[idx+1:]
		} else {
			name = fieldContent
		}

		if name == "" {
			return nil, fmt.Errorf("trollsift: empty field name in pattern %q", pattern)
		}

		fs, err := parseFieldSpec(name, spec)
		if err != nil {
			return nil, err
		}
		segs = append(segs, segment{isField: true, field: fs})
	}
	return segs, nil
}

// Validate checks the pattern for semantic correctness.
// New already calls Validate, so explicit calls are rarely needed.
func (p *Parser) Validate() error {
	for _, seg := range p.segments {
		if seg.isField && seg.field.kind == kindTime && seg.field.ldml == "" {
			return fmt.Errorf("trollsift: time field %q has empty LDML format", seg.field.name)
		}
	}
	return nil
}

// Parse extracts field values from s according to the pattern.
// Time values are stored in UTC internally.
func (p *Parser) Parse(s string) (map[string]Value, error) {
	reStr, fields, err := buildParseRegex(p.segments)
	if err != nil {
		return nil, err
	}

	re, err := regexp.Compile("^" + reStr + "$")
	if err != nil {
		return nil, fmt.Errorf("trollsift: internal regex error: %w", err)
	}

	match := re.FindStringSubmatch(s)
	if match == nil {
		return nil, fmt.Errorf("trollsift: %q does not match pattern %q", s, p.pattern)
	}

	result := make(map[string]Value, len(fields))
	for i, fs := range fields {
		captured := match[i+1]
		switch fs.kind {
		case kindStr:
			result[fs.name] = Value{Raw: captured, Str: captured, IsStr: true}
		case kindInt:
			n, err := strconv.Atoi(strings.TrimSpace(captured))
			if err != nil {
				return nil, fmt.Errorf("trollsift: field %q: parse int %q: %w", fs.name, captured, err)
			}
			result[fs.name] = Value{Raw: captured, Int: n, IsInt: true}
		case kindTime:
			t, err := parseTimeField(fs.ldml, captured, fs.tz)
			if err != nil {
				return nil, fmt.Errorf("trollsift: field %q: parse time %q: %w", fs.name, captured, err)
			}
			result[fs.name] = Value{Raw: captured, Time: t, IsTime: true}
		}
	}
	return result, nil
}

// Compose formats vals into the pattern string.
// If allowPartial is true, fields missing from vals keep their original placeholder.
// If allowPartial is false, any missing field causes an error.
func (p *Parser) Compose(vals map[string]Value, allowPartial bool) (string, error) {
	var sb strings.Builder
	for _, seg := range p.segments {
		if !seg.isField {
			sb.WriteString(seg.literal)
			continue
		}

		fs := seg.field
		val, ok := vals[fs.name]
		if !ok {
			if allowPartial {
				sb.WriteByte('{')
				sb.WriteString(fs.name)
				// Preserve spec only for non-default string fields.
				if fs.kind != kindStr || fs.width != 0 {
					sb.WriteByte(':')
					sb.WriteString(fieldSpecString(fs))
				}
				sb.WriteByte('}')
				continue
			}
			return "", fmt.Errorf("trollsift: missing field %q", fs.name)
		}

		formatted, err := formatValue(val, fs)
		if err != nil {
			return "", err
		}
		sb.WriteString(formatted)
	}
	return sb.String(), nil
}

// Globify converts the pattern to a doublestar-compatible glob string.
// Each field is replaced by '*' (variable-width) or one '?' per character (fixed-width).
// Literal characters, including '/' path separators, are preserved unchanged.
func (p *Parser) Globify() string {
	var sb strings.Builder
	for _, seg := range p.segments {
		if !seg.isField {
			sb.WriteString(seg.literal)
			continue
		}
		fs := seg.field
		switch fs.kind {
		case kindStr, kindInt:
			if fs.width > 0 {
				for i := 0; i < fs.width; i++ {
					sb.WriteByte('?')
				}
			} else {
				sb.WriteByte('*')
			}
		case kindTime:
			sb.WriteString(ldmlToGlobStr(fs.ldml))
		}
	}
	return sb.String()
}
