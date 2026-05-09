// Package runtime owns process lifecycle: startup gate sequence, signal
// handling, drain semantics, and exit-code mapping. Concrete behaviour
// is pinned in specs/001-active-active-pg-proxy/contracts/lifecycle.md.
package runtime

// Exit codes are stable — operators MAY wire them into supervisor
// restart policies. Renames or value changes are MAJOR-version events.
//
// Codes 64-79 occupy the conventional sysexits.h band where the
// meaning maps; EX_CONTROL is outside the band because no standard
// sysexits mapping fits LCM-pipeline init (contracts/lifecycle.md).
const (
	ExitOK           = 0  // Clean shutdown.
	ExitObs          = 74 // Observability bootstrap failed.
	ExitDeps         = 75 // External dependency unavailable (NATS / adapter / executor / manager).
	ExitListen       = 76 // Data-plane proxy listener could not bind.
	ExitSingleton    = 77 // Singleton-claim retry budget exhausted (pg-manager FR-007).
	ExitConfig       = 78 // Configuration error (parse / validate / unknown flag / inline secret).
	ExitDrainTimeout = 79 // Shutdown drain budget exceeded.
	ExitInternal     = 80 // Unexpected internal failure (panic recovered at top-level).
	ExitControl      = 81 // Control-plane bind failed OR initial LCM-audit emit failed.
)

// ExitName returns a stable lowercase identifier for the given code,
// suitable for log fields and supervisor configuration.
func ExitName(code int) string {
	switch code {
	case ExitOK:
		return "ok"
	case ExitObs:
		return "obs"
	case ExitDeps:
		return "deps"
	case ExitListen:
		return "listen"
	case ExitSingleton:
		return "singleton"
	case ExitConfig:
		return "config"
	case ExitDrainTimeout:
		return "drain_timeout"
	case ExitInternal:
		return "internal"
	case ExitControl:
		return "control"
	default:
		return "unknown"
	}
}
