// DumpManifest assembly (T066 / FR-035). The manifest is the canonical
// index of every slice the collector attempted: name, outcome, duration,
// and (on failure) the error block. Operators reading a dump consult
// manifest.json first to decide which slices are usable.

package dump

import (
	"runtime"
	"time"
)

// Manifest is the on-disk DumpManifest persisted at manifest.json
// inside the dump tar/tar.gz. Schema is `pgmctl/v1`; renames are
// MINOR-version events per Constitution V.
type Manifest struct {
	APIVersion   string         `json:"apiVersion"`
	Kind         string         `json:"kind"`
	Pgmctl       BuildInfo      `json:"pgmctl"`
	PgmanProxy   ProxyBuildInfo `json:"pgman_proxy"`
	CapturedAt   Window         `json:"captured_at"`
	RedactLevel  string         `json:"redact_level"`
	ClusterID    string         `json:"cluster_id"`
	Endpoint     string         `json:"endpoint,omitempty"`
	Slices       []SliceEntry   `json:"slices"`
}

// BuildInfo records the dump-time pgmctl binary identity.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
}

// ProxyBuildInfo records the server-side identity observed at dump
// time. Filled from the version probe before slice collection starts.
type ProxyBuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Window is the [started, ended] capture interval.
type Window struct {
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
}

// SliceEntry is the manifest's per-slice outcome row.
type SliceEntry struct {
	Name       string     `json:"name"`
	Outcome    string     `json:"outcome"`
	DurationMS int64      `json:"duration_ms"`
	Path       string     `json:"path,omitempty"`
	Error      *ErrorInfo `json:"error,omitempty"`
}

// ErrorInfo records why a slice failed; mirrors the LCM error envelope.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Outcome values.
const (
	OutcomeOK      = "ok"
	OutcomePartial = "partial"
	OutcomeFailed  = "failed"
)

// NewManifest seeds a manifest with the build identity and the
// supplied start time. Call FinishedAt before serialising.
func NewManifest(pgmctlBuild BuildInfo, redactLevel, clusterID, endpoint string, started time.Time) *Manifest {
	if pgmctlBuild.GoVersion == "" {
		pgmctlBuild.GoVersion = runtime.Version()
	}
	return &Manifest{
		APIVersion:  "pgmctl/v1",
		Kind:        "DumpManifest",
		Pgmctl:      pgmctlBuild,
		CapturedAt:  Window{Started: started.UTC()},
		RedactLevel: redactLevel,
		ClusterID:   clusterID,
		Endpoint:    endpoint,
	}
}

// AddSlice appends a SliceEntry; preserves insertion order so the
// manifest reads top-to-bottom in the same order the collector dispatched.
func (m *Manifest) AddSlice(e SliceEntry) {
	m.Slices = append(m.Slices, e)
}

// SetProxyBuild records the version probe result. Tolerates missing
// data (operator might run dump against a peer with version skew or a
// stub server) — both fields default to "" rather than blocking the
// dump.
func (m *Manifest) SetProxyBuild(b ProxyBuildInfo) {
	m.PgmanProxy = b
}

// FinishedAt stamps the end of the capture window. Idempotent —
// callers may call it more than once (e.g. error path before
// finalisation); the latest call wins.
func (m *Manifest) FinishedAt(t time.Time) {
	m.CapturedAt.Ended = t.UTC()
}

// Outcome computes the overall manifest outcome from per-slice
// entries: any `failed` slice rolls up to `partial`; all-`ok` rolls up
// to `ok`. Used to choose the dump command's exit code (EX_PARTIAL=3
// vs EX_OK=0, FR-037).
func (m *Manifest) Outcome() string {
	allOK := true
	for _, s := range m.Slices {
		if s.Outcome == OutcomeFailed || s.Outcome == OutcomePartial {
			allOK = false
			break
		}
	}
	if allOK {
		return OutcomeOK
	}
	return OutcomePartial
}
