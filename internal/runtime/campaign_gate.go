// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// pgdataComplete reports whether dataDir holds a complete PostgreSQL
// cluster directory: both PG_VERSION and global/pg_control exist as
// regular files.
//
// PG_VERSION alone is not enough. A clone interrupted by a process
// crash leaves a directory that already has PG_VERSION, but the base
// backup protocol sends global/pg_control last, so its absence marks
// the clone as incomplete (specs/005-scale-out-1-to-3/research.md
// R-003). Orderly clone failures already remove the target directory
// inside pg-manager's executor.
//
// A missing file means "not complete" with a nil error; any other stat
// failure is returned, and callers treat it as not complete
// (fail-closed, FR-006). pg-manager has an equivalent internal check;
// contracts/upstream-015.md asks for it to be exported so this
// function can be deleted.
func pgdataComplete(dataDir string) (bool, error) {
	if dataDir == "" {
		return false, nil
	}
	for _, rel := range []string{"PG_VERSION", filepath.Join("global", "pg_control")} {
		fi, err := os.Stat(filepath.Join(dataDir, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf("stat %s: %w", rel, err)
		}
		if !fi.Mode().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}
