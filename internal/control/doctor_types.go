// Doctor types — shared between the registry (doctor_checks.go,
// doctor_fixes.go) and the HTTP handler (handlers_doctor.go). Wire
// shapes mirror contracts/control-plane-extensions.md § 2 and the
// CheckResult / SuggestedFix entries in data-model.md.

package control

import "time"

// Severity is the wire form of a check result's status. Stable —
// renames are MINOR-version events per Constitution V.
type Severity string

// Severity enumeration. Ordering reflects urgency.
const (
	SeverityPass    Severity = "PASS"
	SeverityInfo    Severity = "INFO"
	SeverityWarn    Severity = "WARN"
	SeverityFail    Severity = "FAIL"
	SeverityUnknown Severity = "UNKNOWN"
)

// BlastRadius classifies a SuggestedFix into a confirmation-prompt
// routing tier (data-model.md § SuggestedFix).
type BlastRadius string

// BlastRadius enumeration.
const (
	BlastSingleResource   BlastRadius = "single-resource"
	BlastClusterAffecting BlastRadius = "cluster-affecting"
	BlastAdvisory         BlastRadius = "advisory"
)

// SuggestedFix is the server-published shape served on
// /v1/doctor/checks and embedded in failing CheckResults.
type SuggestedFix struct {
	Name           string      `json:"name"`
	Description    string      `json:"description"`
	BlastRadius    BlastRadius `json:"blast_radius"`
	AppliesToCheck string      `json:"applies_to_check"`
	ArgsSchema     string      `json:"args_schema,omitempty"`
	ApplyEndpoint  string      `json:"apply_endpoint"`
}

// DoctorCheck is the catalog entry served on GET /v1/doctor/checks.
type DoctorCheck struct {
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	SuggestedFix   *SuggestedFix `json:"suggested_fix,omitempty"`
	EvidenceSchema string        `json:"evidence_schema,omitempty"`
}

// CheckResult is the per-check outcome of POST /v1/doctor/run.
type CheckResult struct {
	Name         string         `json:"name"`
	Status       Severity       `json:"status"`
	Message      string         `json:"message,omitempty"`
	Evidence     map[string]any `json:"evidence,omitempty"`
	SuggestedFix *SuggestedFix  `json:"suggested_fix,omitempty"`
	ExecutedAt   time.Time      `json:"executed_at"`
	NodeID       string         `json:"node_id,omitempty"`
}

// DoctorChecksResponse is the wire shape of GET /v1/doctor/checks.
type DoctorChecksResponse struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Checks     []DoctorCheck `json:"checks"`
}

// DoctorReport is the wire shape of POST /v1/doctor/run.
type DoctorReport struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	CapturedAt time.Time     `json:"captured_at"`
	Summary    DoctorSummary `json:"summary"`
	Checks     []CheckResult `json:"checks"`
}

// DoctorSummary is the count-per-severity rollup carried by DoctorReport.
type DoctorSummary struct {
	Pass    int `json:"pass"`
	Info    int `json:"info"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Unknown int `json:"unknown"`
}

// summariseChecks computes DoctorSummary from a slice of CheckResults.
// Side effect: results carrying SuggestedFix on non-FAIL severities are
// left as-is (so WARN with a fixable hint surfaces in the report).
func summariseChecks(checks []CheckResult) DoctorSummary {
	var s DoctorSummary
	for _, c := range checks {
		switch c.Status {
		case SeverityPass:
			s.Pass++
		case SeverityInfo:
			s.Info++
		case SeverityWarn:
			s.Warn++
		case SeverityFail:
			s.Fail++
		default:
			s.Unknown++
		}
	}
	return s
}
