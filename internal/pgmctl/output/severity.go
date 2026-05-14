package output

// Severity is the doctor / cluster-health severity grading. Values
// match the JSON wire form documented in data-model.md.
type Severity string

const (
	SevPass    Severity = "PASS"
	SevInfo    Severity = "INFO"
	SevWarn    Severity = "WARN"
	SevFail    Severity = "FAIL"
	SevUnknown Severity = "UNKNOWN"
)

// Marker returns the [OK]/[INFO]/[WARN]/[FAIL]/[UNKNOWN] form used in
// no-color rendering. Severity wire values are stable per FR-038.
func (s Severity) Marker() string {
	switch s {
	case SevPass:
		return "[OK]"
	case SevInfo:
		return "[INFO]"
	case SevWarn:
		return "[WARN]"
	case SevFail:
		return "[FAIL]"
	case SevUnknown:
		return "[UNKNOWN]"
	default:
		return "[?]"
	}
}

// Color paints s according to the severity → color map documented in
// data-model.md § Severity. Returns the input verbatim when color is
// disabled.
func (s Severity) Color(c *Color, text string) string {
	if c == nil || c.Disabled() {
		return text
	}
	switch s {
	case SevPass:
		return c.Green(text)
	case SevInfo, SevWarn, SevUnknown:
		return c.Yellow(text)
	case SevFail:
		return c.Red(text)
	default:
		return text
	}
}
