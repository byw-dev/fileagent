package trollsift

import (
	"fmt"
	"strconv"
)

// formatValue formats v according to the field spec fs.
func formatValue(v Value, fs fieldSpec) (string, error) {
	switch fs.kind {
	case kindStr:
		if !v.IsStr {
			return "", fmt.Errorf("trollsift: field %q expects a string value", fs.name)
		}
		return v.Str, nil

	case kindInt:
		if !v.IsInt {
			return "", fmt.Errorf("trollsift: field %q expects an integer value", fs.name)
		}
		if fs.width > 0 {
			// Both Nd and 0Nd use zero-padding so the output matches the parse regex [0-9]{N}.
			return fmt.Sprintf("%0*d", fs.width, v.Int), nil
		}
		return strconv.Itoa(v.Int), nil

	case kindTime:
		if !v.IsTime {
			return "", fmt.Errorf("trollsift: field %q expects a time value", fs.name)
		}
		return formatTimeField(v.Time, fs.ldml, fs.tz), nil
	}
	return "", fmt.Errorf("trollsift: unknown field kind for %q", fs.name)
}
