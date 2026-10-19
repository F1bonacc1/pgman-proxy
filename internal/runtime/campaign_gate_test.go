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

type fakeCampaigner bool

func (f fakeCampaigner) ShouldCampaign() bool { return bool(f) }

// TestCampaignGate pins WI-C1 (spec 005 FR-006, F12): before
// manager.New publishes the Manager, pg-manager's nil-receiver
// ShouldCampaign returns true, so an empty node used to campaign during
// boot. The boot-window rule lets only a node with a complete database
// campaign; once the Manager exists its decision is final.
func TestCampaignGate(t *testing.T) {
	complete := mkPGDATA(t, true, true)
	partial := mkPGDATA(t, true, false)

	cases := []struct {
		name     string
		stopping bool
		m        campaigner
		dir      string
		want     bool
	}{
		{"boot window, complete PGDATA", false, nil, complete, true},
		{"boot window, empty PGDATA", false, nil, t.TempDir(), false},
		{"boot window, interrupted clone", false, nil, partial, false},
		{"boot window, data dir unset", false, nil, "", false},
		{"manager says campaign", false, fakeCampaigner(true), t.TempDir(), true},
		{"manager says defer despite PGDATA", false, fakeCampaigner(false), complete, false},
		// Issue #11: a node that is shutting down never campaigns.
		{"shutting down, manager says campaign", true, fakeCampaigner(true), complete, false},
		{"shutting down, boot window, complete PGDATA", true, nil, complete, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newBootWindowGate(c.dir, nil)
			if got := campaignGate(c.stopping, c.m, g.allow); got != c.want {
				t.Fatalf("campaignGate = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBootWindowGateUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := mkPGDATA(t, true, true)
	global := filepath.Join(dir, "global")
	if err := os.Chmod(global, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(global, 0o700) })
	g := newBootWindowGate(dir, nil)
	if g.allow() {
		t.Fatal("an unreadable data dir must not campaign (fail-closed)")
	}
	if got := g.lastReason(); got != "pgdata_unreadable" {
		t.Fatalf("reason = %q, want pgdata_unreadable", got)
	}
}
