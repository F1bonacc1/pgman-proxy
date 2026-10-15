// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

const hbaBlock = "postgres.hba_extras"

func writeConf(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pg_hba.conf")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func readConf(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	beginHBA = "# BEGIN pgman-proxy managed: postgres.hba_extras\n"
	endHBA   = "# END pgman-proxy managed: postgres.hba_extras\n"
)

// TestApplyManagedBlock walks the semantics table of
// specs/005-scale-out-1-to-3/contracts/managed-conf-blocks.md.
func TestApplyManagedBlock(t *testing.T) {
	cases := []struct {
		name       string
		before     string
		lines      []string
		wantAction BlockAction
		want       string
	}{
		{
			name:       "insert into a file without a block",
			before:     "local all all trust\n",
			lines:      []string{"host replication all 10.0.0.0/24 trust"},
			wantAction: BlockInserted,
			want:       "local all all trust\n" + beginHBA + "host replication all 10.0.0.0/24 trust\n" + endHBA,
		},
		{
			name:       "insert into a file without a trailing newline",
			before:     "local all all trust",
			lines:      []string{"host replication all 10.0.0.0/24 trust"},
			wantAction: BlockInserted,
			want:       "local all all trust\n" + beginHBA + "host replication all 10.0.0.0/24 trust\n" + endHBA,
		},
		{
			name:       "update a block in place, bytes around it untouched",
			before:     "a\n" + beginHBA + "old\n" + endHBA + "z\r\n",
			lines:      []string{"new1", "new2"},
			wantAction: BlockUpdated,
			want:       "a\n" + beginHBA + "new1\nnew2\n" + endHBA + "z\r\n",
		},
		{
			name:       "remove a block when the config is empty",
			before:     "a\n" + beginHBA + "old\n" + endHBA + "z\n",
			lines:      nil,
			wantAction: BlockRemoved,
			want:       "a\nz\n",
		},
		{
			name:       "same body is unchanged",
			before:     "a\n" + beginHBA + "same\n" + endHBA,
			lines:      []string{"same"},
			wantAction: BlockUnchanged,
			want:       "a\n" + beginHBA + "same\n" + endHBA,
		},
		{
			name:       "no block and no lines is unchanged",
			before:     "a\n",
			lines:      nil,
			wantAction: BlockUnchanged,
			want:       "a\n",
		},
		{
			name:       "CRLF markers are recognized",
			before:     "a\r\n# BEGIN pgman-proxy managed: postgres.hba_extras\r\nsame\r\n# END pgman-proxy managed: postgres.hba_extras\r\n",
			lines:      []string{"same"},
			wantAction: BlockUnchanged,
			want:       "a\r\n# BEGIN pgman-proxy managed: postgres.hba_extras\r\nsame\r\n# END pgman-proxy managed: postgres.hba_extras\r\n",
		},
		{
			// Blocks appended by earlier releases' PostInitDB hook are
			// never touched; the managed block goes after them.
			name:       "legacy block is left in place",
			before:     "a\n# pgman-proxy: postgres.hba_extras\nhost replication all 10.0.0.0/24 trust\n",
			lines:      []string{"host replication all 10.0.0.0/24 trust"},
			wantAction: BlockInserted,
			want: "a\n# pgman-proxy: postgres.hba_extras\nhost replication all 10.0.0.0/24 trust\n" +
				beginHBA + "host replication all 10.0.0.0/24 trust\n" + endHBA,
		},
		{
			name:       "an empty file gets just the block",
			before:     "",
			lines:      []string{"x"},
			wantAction: BlockInserted,
			want:       beginHBA + "x\n" + endHBA,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeConf(t, c.before, 0o600)
			got, err := applyManagedBlock(p, hbaBlock, c.lines)
			if err != nil {
				t.Fatalf("applyManagedBlock: %v", err)
			}
			if got != c.wantAction {
				t.Fatalf("action = %q, want %q", got, c.wantAction)
			}
			if body := readConf(t, p); body != c.want {
				t.Fatalf("file =\n%q\nwant\n%q", body, c.want)
			}
			// Idempotent: a second apply with the same lines is a no-op.
			again, err := applyManagedBlock(p, hbaBlock, c.lines)
			if err != nil || again != BlockUnchanged {
				t.Fatalf("second apply = %q, %v; want unchanged", again, err)
			}
		})
	}
}

func TestApplyManagedBlockMalformed(t *testing.T) {
	for name, content := range map[string]string{
		"begin without end": "a\n" + beginHBA + "x\n",
		"end without begin": "a\nx\n" + endHBA,
		"two begins":        beginHBA + "x\n" + endHBA + beginHBA + "y\n" + endHBA,
		"end before begin":  endHBA + "x\n" + beginHBA,
	} {
		t.Run(name, func(t *testing.T) {
			p := writeConf(t, content, 0o600)
			if _, err := applyManagedBlock(p, hbaBlock, []string{"z"}); err == nil {
				t.Fatal("want a malformed-block error")
			}
			if got := readConf(t, p); got != content {
				t.Fatalf("file changed on error:\n%q", got)
			}
		})
	}
}

func TestApplyManagedBlockPreservesMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640} {
		p := writeConf(t, "a\n", mode)
		if _, err := applyManagedBlock(p, hbaBlock, []string{"x"}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("mode = %v, want %v", fi.Mode().Perm(), mode)
		}
	}
}

func TestApplyManagedBlockUnchangedDoesNotWrite(t *testing.T) {
	p := writeConf(t, "a\n"+beginHBA+"x\n"+endHBA, 0o600)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManagedBlock(p, hbaBlock, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("mtime changed on an unchanged block: %v", fi.ModTime())
	}
}

func TestApplyManagedBlockReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	p := writeConf(t, "a\n", 0o600)
	dir := filepath.Dir(p)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := applyManagedBlock(p, hbaBlock, []string{"x"}); err == nil {
		t.Fatal("want an error when the temp file cannot be created")
	}
	if got := readConf(t, p); got != "a\n" {
		t.Fatalf("original changed: %q", got)
	}
}

func TestApplyManagedConf(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"postgresql.conf", "pg_hba.conf"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("# initdb\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{}
	cfg.Postgres.ConfExtras = []string{"wal_keep_size = 128MB"}
	cfg.Postgres.HBAExtras = []string{"host replication all 0.0.0.0/0 trust"}

	changes, err := applyManagedConf(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want 2", changes)
	}
	if !strings.Contains(readConf(t, filepath.Join(dir, "postgresql.conf")), "# BEGIN pgman-proxy managed: postgres.conf_extras\nwal_keep_size = 128MB\n") {
		t.Fatal("postgresql.conf block missing")
	}
	if !strings.Contains(readConf(t, filepath.Join(dir, "pg_hba.conf")), beginHBA+"host replication all 0.0.0.0/0 trust\n"+endHBA) {
		t.Fatal("pg_hba.conf block missing")
	}

	if changes, err = applyManagedConf(cfg, dir); err != nil || len(changes) != 0 {
		t.Fatalf("second apply = %+v, %v; want no changes", changes, err)
	}

	// Withdrawing the config removes both blocks.
	changes, err = applyManagedConf(config.Config{}, dir)
	if err != nil || len(changes) != 2 {
		t.Fatalf("withdraw = %+v, %v; want 2 removals", changes, err)
	}
	for _, c := range changes {
		if c.Action != BlockRemoved {
			t.Fatalf("withdraw action = %q, want removed", c.Action)
		}
	}
	if got := readConf(t, filepath.Join(dir, "pg_hba.conf")); got != "# initdb\n" {
		t.Fatalf("pg_hba.conf after withdraw = %q", got)
	}
}
