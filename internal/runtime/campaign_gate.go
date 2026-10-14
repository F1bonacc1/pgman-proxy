// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	pgmanager "github.com/f1bonacc1/pg-manager"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
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

// campaigner is the part of *manager.Manager the lease gate consults.
type campaigner interface {
	ShouldCampaign() bool
}

// campaignGate decides whether this node may contest the leadership
// lease. Once the Manager exists its ShouldCampaign is final (switchover
// deference, crashed-ex-primary health, and from pg-manager 015 the
// empty-node deference). Before that, during the boot window, boot
// decides: pg-manager's nil-receiver ShouldCampaign returns true, which
// let an empty node win the lease and initialize a new database while
// node-a, the only node with data, was still booting (spec 005 F4,
// F12). Callers MUST pass a nil interface, not a typed nil pointer,
// while the Manager is unset.
func campaignGate(m campaigner, boot func() bool) bool {
	if m != nil {
		return m.ShouldCampaign()
	}
	return boot()
}

// bootWindowGate is the boot-window rule: only a node whose data
// directory holds a complete database may campaign. Inspection errors
// defer (fail-closed, FR-006). Transitions are logged once each.
type bootWindowGate struct {
	dataDir string
	logger  *obs.Logger

	mu     sync.Mutex
	logged string // last reason logged ("" = allowed / nothing logged yet)
}

func newBootWindowGate(dataDir string, logger *obs.Logger) *bootWindowGate {
	return &bootWindowGate{dataDir: dataDir, logger: logger}
}

func (g *bootWindowGate) allow() bool {
	complete, err := pgdataComplete(g.dataDir)
	reason := ""
	switch {
	case err != nil:
		reason = "pgdata_unreadable"
	case !complete:
		reason = "pgdata_empty"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if reason != g.logged {
		if g.logger != nil {
			if reason != "" {
				fields := []pgmanager.Field{
					{Key: "reason", Value: reason},
					{Key: "data_dir", Value: g.dataDir},
				}
				if err != nil {
					fields = append(fields, pgmanager.Field{Key: "error", Value: err.Error()})
				}
				g.logger.Info("campaign: boot-window deferral", fields...)
			} else {
				g.logger.Info("campaign: boot-window deferral released",
					pgmanager.Field{Key: "data_dir", Value: g.dataDir})
			}
		}
		g.logged = reason
	}
	return reason == ""
}

// lastReason reports the most recent deferral reason ("" when allowed).
func (g *bootWindowGate) lastReason() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.logged
}
