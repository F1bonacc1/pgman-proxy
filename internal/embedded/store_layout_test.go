// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package embedded

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var archiveNow = time.Date(2026, 10, 15, 9, 15, 0, 0, time.UTC)

// mkStore lays out <dir>/jetstream in the given layout, with one file
// so contents can be checked after a move.
func mkStore(t *testing.T, dir string, layout StoreLayout) {
	t.Helper()
	var sub string
	switch layout {
	case StoreLayoutNone:
		return
	case StoreLayoutClustered:
		sub = filepath.Join("jetstream", "$SYS", "_js_", "_meta_")
	case StoreLayoutStandalone:
		sub = filepath.Join("jetstream", "$G", "streams", "KV_pgmgr_x")
	}
	if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jetstream", "marker"), []byte(layout), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectStoreLayout(t *testing.T) {
	for _, want := range []StoreLayout{StoreLayoutNone, StoreLayoutStandalone, StoreLayoutClustered} {
		t.Run(string(want), func(t *testing.T) {
			dir := t.TempDir()
			mkStore(t, dir, want)
			got, err := DetectStoreLayout(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("DetectStoreLayout = %q, want %q", got, want)
			}
		})
	}
	t.Run("in-memory (empty dir)", func(t *testing.T) {
		got, err := DetectStoreLayout("")
		if err != nil || got != StoreLayoutNone {
			t.Fatalf("DetectStoreLayout(\"\") = %q, %v; want none, nil", got, err)
		}
	})
}

func TestArchiveIncompatibleStore(t *testing.T) {
	cases := []struct {
		name      string
		layout    StoreLayout
		clustered bool
		wantMoved bool
		wantName  string
	}{
		{"standalone → clustered start", StoreLayoutStandalone, true, true, "jetstream.standalone-20261015T091500Z"},
		{"clustered → standalone start (rollback)", StoreLayoutClustered, false, true, "jetstream.clustered-20261015T091500Z"},
		{"standalone, standalone start", StoreLayoutStandalone, false, false, ""},
		{"clustered, clustered start", StoreLayoutClustered, true, false, ""},
		{"no store, clustered start", StoreLayoutNone, true, false, ""},
		{"no store, standalone start", StoreLayoutNone, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			mkStore(t, dir, c.layout)
			before, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}

			to, archived, err := ArchiveIncompatibleStore(dir, c.clustered, archiveNow)
			if err != nil {
				t.Fatalf("ArchiveIncompatibleStore: %v", err)
			}
			if !c.wantMoved {
				if to != "" || archived != StoreLayoutNone {
					t.Fatalf("moved %q (%s), want no-op", to, archived)
				}
				if c.layout != StoreLayoutNone {
					if got, _ := DetectStoreLayout(dir); got != c.layout {
						t.Fatalf("layout after no-op = %q, want %q", got, c.layout)
					}
				}
				return
			}

			if want := filepath.Join(dir, c.wantName); to != want {
				t.Fatalf("to = %q, want %q", to, want)
			}
			if archived != c.layout {
				t.Fatalf("archived = %q, want %q", archived, c.layout)
			}
			if b, err := os.ReadFile(filepath.Join(to, "marker")); err != nil || StoreLayout(b) != c.layout {
				t.Fatalf("archived contents: marker=%q err=%v", b, err)
			}
			if got, _ := DetectStoreLayout(dir); got != StoreLayoutNone {
				t.Fatalf("layout after archive = %q, want none", got)
			}
			// The configured dir is often a mount point: same inode and
			// mode, never renamed.
			after, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("store dir changed: %v → %v", before.Mode(), after.Mode())
			}
			// Idempotent: a second boot finds nothing to move.
			to2, archived2, err := ArchiveIncompatibleStore(dir, c.clustered, archiveNow)
			if err != nil || to2 != "" || archived2 != StoreLayoutNone {
				t.Fatalf("second call = %q, %q, %v; want no-op", to2, archived2, err)
			}
		})
	}
}

func TestArchiveIncompatibleStoreInMemory(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		to, archived, err := ArchiveIncompatibleStore("", clustered, archiveNow)
		if err != nil || to != "" || archived != StoreLayoutNone {
			t.Fatalf("clustered=%v: got %q, %q, %v; want no-op", clustered, to, archived, err)
		}
	}
}

func TestArchiveIncompatibleStoreNameCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "jetstream.standalone-20261015T091500Z"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "jetstream.standalone-20261015T091500Z-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	mkStore(t, dir, StoreLayoutStandalone)
	to, _, err := ArchiveIncompatibleStore(dir, true, archiveNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "jetstream.standalone-20261015T091500Z-2"); to != want {
		t.Fatalf("to = %q, want %q", to, want)
	}
}

func TestArchiveIncompatibleStoreUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	mkStore(t, dir, StoreLayoutStandalone)
	js := filepath.Join(dir, "jetstream")
	if err := os.Chmod(js, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(js, 0o700) })
	if _, _, err := ArchiveIncompatibleStore(dir, true, archiveNow); err == nil {
		t.Fatal("want an error when the store cannot be inspected (fail-closed)")
	}
}
