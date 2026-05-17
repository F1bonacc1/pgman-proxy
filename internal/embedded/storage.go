package embedded

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// StorageDegradedKind enumerates the documented kinds of JetStream
// durability failure that trigger a self-fence per Constitution III.
// Schema mirrors `contracts/observability.md § embedded_nats.storage_degraded`.
type StorageDegradedKind string

const (
	StorageDiskFull       StorageDegradedKind = "disk_full"
	StoragePathUnwritable StorageDegradedKind = "path_unwritable"
	StorageJSCorruption   StorageDegradedKind = "js_corruption"  // reserved; not surfaced by the v1 monitor
	StorageQuotaExceeded  StorageDegradedKind = "quota_exceeded" // reserved; not surfaced by the v1 monitor
)

// StorageMonitorOptions configures the durability watchdog. Defaults
// produce a sensible 5-second polling interval that scales to the
// quickstart's 60-MB memory budget; callers can tighten the interval
// for tests.
type StorageMonitorOptions struct {
	// Path is the JetStream storage directory to watch. Empty disables
	// the monitor entirely (in-memory single-peer mode has no
	// durability concern).
	Path string

	// PollInterval governs how often the monitor stats the path.
	// Default 5 s.
	PollInterval time.Duration

	// MinFreeBytes is the disk-full threshold. When the filesystem
	// reports fewer free bytes than this on the storage path, the
	// monitor emits a `storage_degraded{kind=disk_full}` event and
	// invokes the self-fence callback. Default 64 MiB — large enough
	// to ride out a brief log spike, small enough to leave the
	// operator a recovery window before the disk is fully exhausted.
	MinFreeBytes uint64
}

// StorageMonitor watches a JetStream storage directory for the
// degradation conditions documented in
// `contracts/observability.md`. On the first detected degradation it
// emits the structured event AND invokes the supplied self-fence
// callback (Constitution III). The monitor is idempotent — once
// degradation has been signalled, subsequent polls do not re-fire
// the event until the path recovers and the operator clears the
// state by restarting the peer.
type StorageMonitor struct {
	srv     *Server
	opts    StorageMonitorOptions
	fence   SelfFence
	tripped atomic.Bool
}

// SelfFence is the callback the monitor invokes when degradation is
// detected. Its job is to release the pg-manager leadership lease so
// the peer stops serving writes (Constitution III). The host wires
// this to the `cluster.Handles.Leadership.Close()` call site.
type SelfFence func(ctx context.Context, kind StorageDegradedKind, path string, err error)

// NewStorageMonitor constructs a monitor. Returns nil when the path
// is empty (in-memory mode); callers can call Start/Stop on a nil
// monitor as no-ops.
func NewStorageMonitor(srv *Server, opts StorageMonitorOptions, fence SelfFence) *StorageMonitor {
	if opts.Path == "" {
		return nil
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.MinFreeBytes == 0 {
		opts.MinFreeBytes = 64 * 1024 * 1024 // 64 MiB
	}
	return &StorageMonitor{srv: srv, opts: opts, fence: fence}
}

// Start runs the polling loop in a goroutine until ctx is cancelled.
// Safe to call on a nil receiver (no-op).
func (m *StorageMonitor) Start(ctx context.Context) {
	if m == nil {
		return
	}
	go m.run(ctx)
}

func (m *StorageMonitor) run(ctx context.Context) {
	t := time.NewTicker(m.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if kind, _, err := m.check(); kind != "" {
				m.trip(ctx, kind, err)
			}
		}
	}
}

// check evaluates the current state of the storage path. Returns a
// non-empty kind only when degradation is detected; the second return
// is the free-bytes count reported by statfs (0 when the path is
// unwritable).
func (m *StorageMonitor) check() (StorageDegradedKind, uint64, error) {
	info, err := os.Stat(m.opts.Path)
	if err != nil {
		return StoragePathUnwritable, 0, err
	}
	if !info.IsDir() {
		return StoragePathUnwritable, 0, fmt.Errorf("path %q is not a directory", m.opts.Path)
	}
	// Probe writability with a small temp file. We use os.OpenFile
	// rather than os.Mkdir so the existing JetStream content isn't
	// disturbed.
	probe := m.opts.Path + "/.pgman-proxy-storage-probe"
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return StoragePathUnwritable, 0, err
	}
	_ = f.Close()
	_ = os.Remove(probe)

	var statfs syscall.Statfs_t
	if err := syscall.Statfs(m.opts.Path, &statfs); err != nil {
		return StoragePathUnwritable, 0, err
	}
	// Bsize is signed on linux/amd64 (int64); cast to uint64 to align
	// with the unsigned Bavail. Both are filesystem-reported byte
	// counts — never negative in practice, so the conversion is sound.
	free := statfs.Bavail * uint64(statfs.Bsize) //nolint:gosec // G115: filesystem byte counts are non-negative
	if free < m.opts.MinFreeBytes {
		return StorageDiskFull, free, fmt.Errorf("free bytes %d below threshold %d", free, m.opts.MinFreeBytes)
	}
	return "", free, nil
}

// trip surfaces the degradation as a structured event, invokes the
// self-fence callback, and marks the monitor tripped so we don't
// re-fire on every subsequent poll.
func (m *StorageMonitor) trip(ctx context.Context, kind StorageDegradedKind, err error) {
	if m.tripped.Swap(true) {
		return
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	if m.srv != nil {
		m.srv.EmitStorageDegraded(string(kind), m.opts.Path, errMsg)
	}
	if m.fence != nil {
		m.fence(ctx, kind, m.opts.Path, err)
	}
}

// Tripped reports whether the monitor has fired a degradation event
// (used by tests + the host's health endpoint).
func (m *StorageMonitor) Tripped() bool {
	if m == nil {
		return false
	}
	return m.tripped.Load()
}

// ErrMonitorDisabled is returned when callers attempt to invoke
// monitor operations on a nil receiver via a non-nil-checking path.
// Currently unused; retained as a sentinel.
var ErrMonitorDisabled = errors.New("embedded storage monitor disabled (in-memory mode)")
