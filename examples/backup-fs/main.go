// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.
//
// Reference filesystem-backed BackupExecutor — out-of-tree by design
// (T067 / FR-030). pgman-proxy itself never bundles a backup backend;
// this example shows how an operator binary can compose pgman-proxy's
// runtime with a custom executor.
//
// Storage layout: every backup gets a directory under -dir of the form
// `<rfc3339>-<random-id>/`, holding a base.tar produced by
// pg_basebackup against the supplied source DSN. Restore copies the
// tarball back. Verify checks the directory is non-empty.
//
// Production-readiness caveats (intentionally NOT addressed here, so
// the example stays small):
//   * No retention policy — operators MUST prune old backups themselves.
//   * No encryption-at-rest — wrap the storage path in dm-crypt /
//     borg / restic if you need it.
//   * No catalog DB — `List` walks the directory tree on every call.
//   * `Verify` only checks for non-emptiness, not tarball validity.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pgmanager "github.com/f1bonacc1/pg-manager"
)

// FSExecutor is a filesystem-backed BackupExecutor.
type FSExecutor struct {
	root     string // root directory holding one subdir per backup
	schedule string // cron expression; empty disables scheduled backups
	pgBinDir string // path to pg_basebackup
}

// NewFSExecutor constructs an FSExecutor. Returns an error when root
// can't be created or pgBinDir doesn't contain pg_basebackup.
func NewFSExecutor(root, schedule, pgBinDir string) (*FSExecutor, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("backup-fs: create root %q: %w", root, err)
	}
	if _, err := os.Stat(filepath.Join(pgBinDir, "pg_basebackup")); err != nil {
		return nil, fmt.Errorf("backup-fs: pg_basebackup not found under %q: %w", pgBinDir, err)
	}
	return &FSExecutor{root: root, schedule: schedule, pgBinDir: pgBinDir}, nil
}

// Schedule returns the cron expression. Empty disables scheduled
// backups while leaving Manager.TriggerBackup callable.
func (e *FSExecutor) Schedule() string { return e.schedule }

// RunBaseBackup runs pg_basebackup against target (a libpq DSN). The
// dsn is the connection string the engine selected as the most-aligned
// healthy candidate (FRs in pg-manager interfaces.go).
func (e *FSExecutor) RunBaseBackup(ctx context.Context, target string) (pgmanager.BackupID, error) {
	id := pgmanager.BackupID(fmt.Sprintf("%s-%s",
		time.Now().UTC().Format("20060102T150405Z"),
		randID(8),
	))
	dest := filepath.Join(e.root, string(id))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", fmt.Errorf("backup-fs: mkdir %q: %w", dest, err)
	}
	args := []string{
		"--dbname=" + target,
		"-D", dest,
		"-Ft", // tar format
		"-z",  // gzip
		"-X", "fetch",
		"-P",
		"-c", "fast",
	}
	cmd := exec.CommandContext(ctx, filepath.Join(e.pgBinDir, "pg_basebackup"), args...) //nolint:gosec
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Best-effort cleanup; leave a marker file so operators can
		// audit failed runs.
		_ = os.WriteFile(filepath.Join(dest, "FAILED"), []byte(err.Error()), 0o600)
		return "", fmt.Errorf("backup-fs: pg_basebackup: %w", err)
	}
	return id, nil
}

// Verify checks the backup directory exists and is non-empty.
func (e *FSExecutor) Verify(_ context.Context, id pgmanager.BackupID) error {
	entries, err := os.ReadDir(filepath.Join(e.root, string(id)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pgmanager.ErrBackupNotFound
		}
		return fmt.Errorf("backup-fs: verify %q: %w", id, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("backup-fs: %q empty", id)
	}
	return nil
}

// List returns all known backups in chronological order (newest first).
func (e *FSExecutor) List(_ context.Context) ([]pgmanager.BackupInfo, error) {
	entries, err := os.ReadDir(e.root)
	if err != nil {
		return nil, fmt.Errorf("backup-fs: list: %w", err)
	}
	out := make([]pgmanager.BackupInfo, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		out = append(out, pgmanager.BackupInfo{
			ID:          pgmanager.BackupID(ent.Name()),
			Path:        filepath.Join(e.root, ent.Name()),
			StartedAt:   info.ModTime(),
			CompletedAt: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CompletedAt.After(out[j].CompletedAt)
	})
	return out, nil
}

// Restore copies the backup tarball to target. Caller is responsible
// for invoking pg_basebackup-style extraction; this example doesn't
// untar to keep the surface minimal.
func (e *FSExecutor) Restore(_ context.Context, id pgmanager.BackupID, target string) error {
	src := filepath.Join(e.root, string(id))
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pgmanager.ErrBackupNotFound
		}
		return fmt.Errorf("backup-fs: stat %q: %w", id, err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("backup-fs: mkdir %q: %w", target, err)
	}
	// Copy each file in src verbatim.
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		srcPath := filepath.Join(src, ent.Name())
		dstPath := filepath.Join(target, ent.Name())
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // operator-supplied paths.
	if err != nil {
		return err
	}
	defer in.Close()                                                        //nolint:errcheck
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck
	_, err = io.Copy(out, in)
	return err
}

func randID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	now := time.Now().UnixNano()
	for i := range b {
		b[i] = alphabet[(now+int64(i*31))%int64(len(alphabet))]
	}
	return string(b)
}

// main is a CLI smoke wrapper so this file compiles standalone (the
// `go run ./examples/backup-fs --help` story). A real operator binary
// would wire FSExecutor into a custom main alongside pgman-proxy's
// runtime; this stub just confirms the executor can be constructed.
func main() {
	root := flag.String("dir", "./backups", "backup root directory")
	pgBinDir := flag.String("pg-bin-dir", "/usr/lib/postgresql/17/bin", "directory holding pg_basebackup")
	schedule := flag.String("schedule", "", "cron expression; empty disables scheduled backups")
	flag.Parse()

	exec, err := NewFSExecutor(*root, *schedule, *pgBinDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup-fs: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("backup-fs ready: root=%s schedule=%q\n", *root, exec.Schedule())
}
