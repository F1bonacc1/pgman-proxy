// Doctor report rendering (T081).
//
// Maps the server's DoctorReport JSON into the operator-facing
// table / JSON / YAML output forms. Severity coloring follows the
// data-model.md § Severity matrix: PASS green, INFO/WARN/UNKNOWN
// yellow, FAIL red.

package doctor

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/pgmctl/output"
)

// Report mirrors the server's wire shape for POST /v1/doctor/run.
// Kept local to avoid pgmctl importing internal/control.
type Report struct {
	APIVersion string    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string    `json:"kind" yaml:"kind"`
	CapturedAt time.Time `json:"captured_at" yaml:"captured_at"`
	Summary    Summary   `json:"summary" yaml:"summary"`
	Checks     []Check   `json:"checks" yaml:"checks"`
}

// Summary mirrors the server's count rollup.
type Summary struct {
	Pass    int `json:"pass" yaml:"pass"`
	Info    int `json:"info" yaml:"info"`
	Warn    int `json:"warn" yaml:"warn"`
	Fail    int `json:"fail" yaml:"fail"`
	Unknown int `json:"unknown" yaml:"unknown"`
}

// Check mirrors the per-result wire shape.
type Check struct {
	Name         string         `json:"name" yaml:"name"`
	Status       string         `json:"status" yaml:"status"`
	Message      string         `json:"message,omitempty" yaml:"message,omitempty"`
	Evidence     map[string]any `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	SuggestedFix *Fix           `json:"suggested_fix,omitempty" yaml:"suggested_fix,omitempty"`
	ExecutedAt   time.Time      `json:"executed_at" yaml:"executed_at"`
	NodeID       string         `json:"node_id,omitempty" yaml:"node_id,omitempty"`
}

// Fix mirrors the suggested-fix wire shape.
type Fix struct {
	Name           string `json:"name" yaml:"name"`
	Description    string `json:"description" yaml:"description"`
	BlastRadius    string `json:"blast_radius" yaml:"blast_radius"`
	AppliesToCheck string `json:"applies_to_check" yaml:"applies_to_check"`
	ApplyEndpoint  string `json:"apply_endpoint" yaml:"apply_endpoint"`
}

// RenderTable prints a single-line-per-check rollup with severity
// markers and a final summary line. Mirrors the doctor table layout
// described in cli-commands.md § doctor.
func RenderTable(w io.Writer, c *output.Color, rep Report) error {
	if len(rep.Checks) == 0 {
		_, err := fmt.Fprintln(w, "no checks ran (empty registry?)")
		return err
	}
	t := output.NewTable("STATUS", "CHECK", "MESSAGE")
	for _, chk := range rep.Checks {
		marker := severityMarker(c, chk.Status)
		t.AddRow(marker, chk.Name, chk.Message)
	}
	if err := t.Render(w); err != nil {
		return err
	}
	// Suggested-fix lines per failing/warning check go below the table
	// — keeping them on their own lines preserves the column layout for
	// the main rollup.
	for _, chk := range rep.Checks {
		if chk.SuggestedFix == nil {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n  ↳ %s: %s\n",
			c.Yellow("suggested fix"),
			chk.SuggestedFix.Description)
		_, _ = fmt.Fprintf(w, "    (name=%s, blast_radius=%s, applies_to=%s)\n",
			chk.SuggestedFix.Name,
			chk.SuggestedFix.BlastRadius,
			chk.SuggestedFix.AppliesToCheck)
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", summaryLine(c, rep.Summary))
	return nil
}

// RenderCatalog prints the discovery catalog returned by GET
// /v1/doctor/checks. Used by `pgmctl doctor --list`.
func RenderCatalog(w io.Writer, c *output.Color, checks []Check) error {
	if len(checks) == 0 {
		_, err := fmt.Fprintln(w, "no checks registered")
		return err
	}
	t := output.NewTable("CHECK", "FIX", "DESCRIPTION")
	for _, chk := range checks {
		fixName := "-"
		if chk.SuggestedFix != nil {
			fixName = chk.SuggestedFix.Name + " (" + chk.SuggestedFix.BlastRadius + ")"
		}
		t.AddRow(chk.Name, fixName, chk.Message)
	}
	return t.Render(w)
}

// severityMarker returns the colored [STATUS] tag used in table output.
// data-model.md § Severity prescribes the green/yellow/red mapping.
func severityMarker(c *output.Color, status string) string {
	upper := strings.ToUpper(status)
	tag := "[" + upper + "]"
	switch upper {
	case "PASS":
		return c.Green(tag)
	case "FAIL":
		return c.Red(tag)
	default: // INFO, WARN, UNKNOWN
		return c.Yellow(tag)
	}
}

// summaryLine renders the trailing "X PASS · Y WARN · …" rollup.
func summaryLine(c *output.Color, s Summary) string {
	parts := []string{}
	if s.Pass > 0 {
		parts = append(parts, c.Green(fmt.Sprintf("%d PASS", s.Pass)))
	}
	if s.Info > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d INFO", s.Info)))
	}
	if s.Warn > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d WARN", s.Warn)))
	}
	if s.Fail > 0 {
		parts = append(parts, c.Red(fmt.Sprintf("%d FAIL", s.Fail)))
	}
	if s.Unknown > 0 {
		parts = append(parts, c.Yellow(fmt.Sprintf("%d UNKNOWN", s.Unknown)))
	}
	if len(parts) == 0 {
		return "no results"
	}
	return strings.Join(parts, " · ")
}

// WorstSeverity returns the most urgent severity in the report (used
// by the CLI to decide exit code: FAIL → EX_UNHEALTHY, UNKNOWN →
// EX_UNKNOWN, etc.). Returns "" for an empty report.
func WorstSeverity(rep Report) string {
	if rep.Summary.Fail > 0 {
		return "FAIL"
	}
	if rep.Summary.Unknown > 0 {
		return "UNKNOWN"
	}
	if rep.Summary.Warn > 0 {
		return "WARN"
	}
	if rep.Summary.Info > 0 {
		return "INFO"
	}
	if rep.Summary.Pass > 0 {
		return "PASS"
	}
	return ""
}
