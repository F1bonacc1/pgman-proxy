// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// BlockAction reports what applyManagedBlock did to a file.
type BlockAction string

// Managed block outcomes (contracts/managed-conf-blocks.md).
const (
	BlockInserted  BlockAction = "inserted"
	BlockUpdated   BlockAction = "updated"
	BlockRemoved   BlockAction = "removed"
	BlockUnchanged BlockAction = "unchanged"
)

// blockChange is one changed managed block, for logging.
type blockChange struct {
	File   string
	Block  string
	Action BlockAction
	Lines  int
}

func blockMarkers(name string) (begin, end string) {
	return "# BEGIN pgman-proxy managed: " + name, "# END pgman-proxy managed: " + name
}

// applyManagedBlock rewrites the marker-delimited block `name` in the
// file at path so its body is exactly lines: inserted at the end when
// absent, replaced in place when present, removed when lines is empty.
// Every byte outside the markers is preserved, the write is atomic
// (temp file in the same directory, fsync, rename) and keeps the file
// mode. Unchanged content is not rewritten. Malformed marker pairs are
// an error and nothing is written: guessing would risk dropping
// operator-owned lines.
func applyManagedBlock(path, name string, lines []string) (BlockAction, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is inside the operator-configured data dir
	if err != nil {
		return "", err
	}
	content := string(raw)
	begin, end := blockMarkers(name)

	// Split keeping terminators so untouched lines round-trip byte for byte.
	parts := strings.SplitAfter(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	bi, ei := -1, -1
	for i, p := range parts {
		switch strings.TrimRight(p, "\r\n \t") {
		case begin:
			if bi >= 0 {
				return "", fmt.Errorf("managed block %s malformed in %s: duplicate BEGIN marker", name, path)
			}
			bi = i
		case end:
			if ei >= 0 {
				return "", fmt.Errorf("managed block %s malformed in %s: duplicate END marker", name, path)
			}
			ei = i
		}
	}
	if (bi < 0) != (ei < 0) || (bi >= 0 && ei < bi) {
		return "", fmt.Errorf("managed block %s malformed in %s: unmatched BEGIN/END markers", name, path)
	}

	block := ""
	if len(lines) > 0 {
		block = begin + "\n" + strings.Join(lines, "\n") + "\n" + end + "\n"
	}

	var next string
	var action BlockAction
	switch {
	case bi < 0 && len(lines) == 0:
		return BlockUnchanged, nil
	case bi < 0:
		next = content
		if next != "" && !strings.HasSuffix(next, "\n") {
			next += "\n"
		}
		next += block
		action = BlockInserted
	default:
		current := make([]string, 0, ei-bi-1)
		for _, p := range parts[bi+1 : ei] {
			current = append(current, strings.TrimRight(p, "\r\n"))
		}
		if len(lines) > 0 && equalLines(current, lines) {
			return BlockUnchanged, nil
		}
		next = strings.Join(parts[:bi], "") + block + strings.Join(parts[ei+1:], "")
		action = BlockUpdated
		if len(lines) == 0 {
			action = BlockRemoved
		}
	}
	if err := writeFileAtomic(path, []byte(next)); err != nil {
		return "", err
	}
	return action, nil
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeFileAtomic replaces path with data via a temp file in the same
// directory, keeping path's permission bits.
func writeFileAtomic(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pgman-proxy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// applyManagedConf applies the postgres.conf_extras and
// postgres.hba_extras blocks into dataDir and returns the blocks that
// changed. It runs after a database is first created and again on
// every boot of a node whose database exists, so an existing single
// node gains the replication rules its new peers need to clone it
// (spec 005 FR-013, F11). Empty lists remove previously managed blocks.
func applyManagedConf(cfg config.Config, dataDir string) ([]blockChange, error) {
	targets := []struct {
		file  string
		block string
		lines []string
	}{
		{"postgresql.conf", "postgres.conf_extras", cfg.Postgres.ConfExtras},
		{"pg_hba.conf", "postgres.hba_extras", cfg.Postgres.HBAExtras},
	}
	var changes []blockChange
	for _, tg := range targets {
		action, err := applyManagedBlock(filepath.Join(dataDir, tg.file), tg.block, tg.lines)
		if err != nil {
			return changes, fmt.Errorf("apply %s to %s: %w", tg.block, tg.file, err)
		}
		if action != BlockUnchanged {
			changes = append(changes, blockChange{File: tg.file, Block: tg.block, Action: action, Lines: len(tg.lines)})
		}
	}
	return changes, nil
}

// logManagedChanges emits postgres.managed_block_applied per changed
// block (contracts/observability.md).
func logManagedChanges(logger *obs.Logger, nodeID, phase string, changes []blockChange, reloaded bool) {
	for _, c := range changes {
		logger.Event("postgres.managed_block_applied", nodeID,
			pgmanager.Field{Key: "file", Value: c.File},
			pgmanager.Field{Key: "block", Value: c.Block},
			pgmanager.Field{Key: "action", Value: string(c.Action)},
			pgmanager.Field{Key: "lines", Value: c.Lines},
			pgmanager.Field{Key: "phase", Value: phase},
			pgmanager.Field{Key: "reloaded", Value: reloaded})
	}
}
