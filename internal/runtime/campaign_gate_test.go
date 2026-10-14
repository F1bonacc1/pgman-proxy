// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// mkPGDATA lays out the files pgdataComplete inspects under a fresh
// temp dir and returns it.
func mkPGDATA(t *testing.T, pgVersion, pgControl bool) string {
	t.Helper()
	dir := t.TempDir()
	if pgVersion {
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if pgControl {
		if err := os.MkdirAll(filepath.Join(dir, "global"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "global", "pg_control"), make([]byte, 8192), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPGDATAComplete(t *testing.T) {
	cases := []struct {
		name string
		dir  func(t *testing.T) string
		want bool
	}{
		{"unset data dir", func(*testing.T) string { return "" }, false},
		{"missing data dir", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, false},
		{"empty data dir", func(t *testing.T) string { return mkPGDATA(t, false, false) }, false},
		// An interrupted clone has PG_VERSION but not global/pg_control,
		// which the base backup protocol sends last (research R-003).
		{"PG_VERSION only (interrupted clone)", func(t *testing.T) string { return mkPGDATA(t, true, false) }, false},
		{"pg_control only", func(t *testing.T) string { return mkPGDATA(t, false, true) }, false},
		{"complete", func(t *testing.T) string { return mkPGDATA(t, true, true) }, true},
		{"pg_control is a directory", func(t *testing.T) string {
			dir := mkPGDATA(t, true, false)
			if err := os.MkdirAll(filepath.Join(dir, "global", "pg_control"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pgdataComplete(c.dir(t))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("pgdataComplete = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPGDATACompleteUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := mkPGDATA(t, true, true)
	global := filepath.Join(dir, "global")
	if err := os.Chmod(global, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(global, 0o700) })
	got, err := pgdataComplete(dir)
	if err == nil {
		t.Fatalf("want an error for an unreadable global/, got complete=%v", got)
	}
	if got {
		t.Fatal("an unreadable data dir must not report complete")
	}
}
