// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"context"
	"sync"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/adapters/osfs"

	"github.com/f1bonacc1/pgman-proxy/internal/obs"
)

// pgdataComplete reports whether dataDir holds a complete PostgreSQL
// cluster directory, using pg-manager's own rule (milestone 015,
// pgmanager.ClassifyDataDir): PG_VERSION and global/pg_control both
// present. A clone interrupted by a crash has PG_VERSION but not
// pg_control, which the base backup protocol sends last (spec 005
// research R-003), so it does not count. One rule on both sides: the
// boot-window gate here and pg-manager's data-state records agree on
// what "holds data" means.
//
// An unset data dir is "not complete" with a nil error; any other
// inspection failure is returned and callers treat it as not complete
// (fail-closed, FR-006).
func pgdataComplete(dataDir string) (bool, error) {
	if dataDir == "" {
		return false, nil
	}
	st, err := pgmanager.ClassifyDataDir(context.Background(), osfs.New(), dataDir)
	if err != nil {
		return false, err
	}
	return st == pgmanager.DataStateComplete, nil
}

// campaigner is the part of *manager.Manager the lease gate consults.
type campaigner interface {
	ShouldCampaign() bool
}

// campaignGate decides whether this node may contest the leadership
// lease. A node that is shutting down never does (issue #11): its lease
// loop runs until the drain's cluster step closes it, and a standby
// winning a freed lease in that window would hold it, with its
// Postgres stopping and nobody to promote, until it let go. The gate
// never affects renewing a lease already held, so a primary keeps its
// lease while its Postgres shuts down.
//
// Otherwise, once the Manager exists its ShouldCampaign is final
// (switchover deference, crashed-ex-primary health, and from pg-manager
// 015 the empty-node deference). Before that, during the boot window,
// boot decides: pg-manager's nil-receiver ShouldCampaign returns true,
// which let an empty node win the lease and initialize a new database
// while node-a, the only node with data, was still booting (spec 005
// F4, F12). Callers MUST pass a nil interface, not a typed nil pointer,
// while the Manager is unset.
func campaignGate(stopping bool, m campaigner, boot func() bool) bool {
	if stopping {
		return false
	}
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
