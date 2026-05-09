// BackupExecutor configuration knob (T066, FR-030).
//
// pgman-proxy itself never bundles a backup backend (FR-030 explicitly
// keeps PITR / restore / a default backend out of scope for v1). When
// the operator hasn't wired a BackupExecutor into Manager.Config,
// `TriggerBackup` MUST be rejected with `backup_executor_missing` so the
// caller sees a structured 412 instead of a silent no-op.
//
// This file holds the operator-facing config surface that lets a future
// custom binary (or the reference filesystem example under
// `examples/backup-fs/`) declare its own backend without polluting the
// shared config struct.

package config

// BackupConfig describes how to wire an external backup executor into
// pgman-proxy. v1 only supports two values for `Driver`: the empty
// string (which leaves Manager.Config.Backup unset and causes
// `TriggerBackup` to return `backup_executor_missing`), and `"none"`
// (an alias for the empty string, accepted so operators can be
// explicit).
//
// Future drivers (e.g. `"pgbackrest"`, `"wal-g"`, `"fs"`) extend this
// surface with their own settings sub-blocks. Validation rejects
// unknown driver values fail-closed (Constitution II).
type BackupConfig struct {
	// Driver names the external backup executor. Empty / "none"
	// disables backup orchestration. Custom values are reserved for
	// out-of-tree operator binaries (see examples/backup-fs/main.go).
	Driver string `yaml:"driver" json:"driver"`
}

// Configured reports whether a backup executor should be wired into
// Manager.Config. Used by runtime/start.go to decide between leaving
// Manager.Config.Backup = nil (so TriggerBackup fails fast per FR-030)
// vs. delegating to a host-provided constructor.
func (b BackupConfig) Configured() bool {
	switch b.Driver {
	case "", "none":
		return false
	}
	return true
}
