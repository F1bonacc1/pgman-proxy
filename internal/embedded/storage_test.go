package embedded

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStorageMonitor_NilOnEmptyPath asserts the in-memory mode
// shortcut: an empty path returns nil and the no-op monitor methods
// are safe to invoke on the nil receiver.
func TestStorageMonitor_NilOnEmptyPath(t *testing.T) {
	m := NewStorageMonitor(nil, StorageMonitorOptions{Path: ""}, nil)
	if m != nil {
		t.Fatalf("expected nil monitor for empty path, got %#v", m)
	}
	// Methods on a nil receiver MUST be safe.
	m.Start(context.Background())
	if m.Tripped() {
		t.Errorf("nil monitor should report Tripped() = false")
	}
}

// TestStorageMonitor_DetectsUnwritablePath simulates a path that
// disappears between checks. The monitor should fire `path_unwritable`,
// invoke the fence callback, and mark itself tripped.
func TestStorageMonitor_DetectsUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	var fenceCalls atomic.Int32
	var fenceKind atomic.Pointer[StorageDegradedKind]
	mon := NewStorageMonitor(nil, StorageMonitorOptions{
		Path:         missing,
		PollInterval: 5 * time.Millisecond,
	}, func(_ context.Context, kind StorageDegradedKind, _ string, _ error) {
		fenceCalls.Add(1)
		k := kind
		fenceKind.Store(&k)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	mon.Start(ctx)

	// Wait for the monitor to trip.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mon.Tripped() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !mon.Tripped() {
		t.Fatal("monitor never tripped on missing path")
	}
	if fenceCalls.Load() == 0 {
		t.Errorf("self-fence callback was never invoked")
	}
	got := fenceKind.Load()
	if got == nil || *got != StoragePathUnwritable {
		t.Errorf("fence kind = %v, want %v", got, StoragePathUnwritable)
	}
}

// TestStorageMonitor_DoesNotRetrip asserts the idempotency rule from
// `contracts/observability.md`: once degradation has been signalled,
// subsequent polls don't re-fire the event.
func TestStorageMonitor_DoesNotRetrip(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "still-missing")
	var fenceCalls atomic.Int32
	mon := NewStorageMonitor(nil, StorageMonitorOptions{
		Path:         missing,
		PollInterval: 2 * time.Millisecond,
	}, func(_ context.Context, _ StorageDegradedKind, _ string, _ error) {
		fenceCalls.Add(1)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	mon.Start(ctx)

	// Sleep long enough for many poll cycles.
	time.Sleep(50 * time.Millisecond)
	if got := fenceCalls.Load(); got != 1 {
		t.Errorf("fence invoked %d times, want exactly 1 (idempotency)", got)
	}
}

// TestStorageMonitor_HealthyPathDoesNotTrip asserts a writable path
// with sufficient free bytes does not falsely fire degradation.
func TestStorageMonitor_HealthyPathDoesNotTrip(t *testing.T) {
	dir := t.TempDir()
	// Use a tiny MinFreeBytes so the temp filesystem can satisfy it.
	mon := NewStorageMonitor(nil, StorageMonitorOptions{
		Path:         dir,
		PollInterval: 5 * time.Millisecond,
		MinFreeBytes: 1, // 1 byte threshold — any non-full FS satisfies
	}, func(_ context.Context, _ StorageDegradedKind, _ string, _ error) {
		t.Errorf("fence callback fired on a healthy path")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	mon.Start(ctx)
	time.Sleep(50 * time.Millisecond)

	if mon.Tripped() {
		t.Error("monitor tripped on healthy path")
	}

	// Stop the monitor (via ctx cancel) and give it a moment to wind
	// down before checking for the probe file. The probe is created
	// and removed within a single check() call, so any leftover would
	// indicate a real failure rather than a race.
	cancel()
	time.Sleep(20 * time.Millisecond)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == ".pgman-proxy-storage-probe" {
			t.Errorf("probe file %q was not cleaned up after monitor stopped", e.Name())
		}
	}
}
