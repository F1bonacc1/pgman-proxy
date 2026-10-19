// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"slices"
	"testing"

	pgmanager "github.com/f1bonacc1/pg-manager"
	"github.com/f1bonacc1/pg-manager/manager"

	"github.com/f1bonacc1/pgman-proxy/internal/cluster"
	"github.com/f1bonacc1/pgman-proxy/internal/embedded"
	"github.com/f1bonacc1/pgman-proxy/internal/runtime"
)

// nopPostgres stands in for the executor; the order tests never run a
// step.
type nopPostgres struct{ pgmanager.PostgresExecutor }

func (nopPostgres) IsRunning(context.Context) (bool, error)        { return false, nil }
func (nopPostgres) Stop(context.Context, pgmanager.StopMode) error { return nil }

func stepNames(steps []runtime.DrainStep) []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names
}

func fullStartupResult() *runtime.StartupResult {
	return &runtime.StartupResult{
		Manager:  &manager.Manager{},
		Postgres: nopPostgres{},
		Cluster:  &cluster.Handles{},
		Embedded: &embedded.Server{},
	}
}

// Issue #11: Postgres must stop after the manager (so the reconciler
// cannot restart it) and before the cluster step releases the lease
// (so no standby promotes before the old primary's WAL has shipped).
func TestBuildShutdownSteps_PostgresBetweenManagerAndLease(t *testing.T) {
	names := stepNames(buildShutdownSteps(fullStartupResult(), true))
	order := []string{"manager", "postgres", "embedded-pre-stop", "cluster", "embedded-nats"}
	last := -1
	for _, name := range order {
		i := slices.Index(names, name)
		if i < 0 {
			t.Fatalf("step %q missing from %v", name, names)
		}
		if i <= last {
			t.Fatalf("step %q out of order in %v; want %v as a subsequence", name, names, order)
		}
		last = i
	}
}

func TestBuildShutdownSteps_PostgresStepOptional(t *testing.T) {
	if names := stepNames(buildShutdownSteps(fullStartupResult(), false)); slices.Contains(names, "postgres") {
		t.Errorf("stopPostgres=false still added the postgres step: %v", names)
	}
	res := fullStartupResult()
	res.Postgres = nil
	if names := stepNames(buildShutdownSteps(res, true)); slices.Contains(names, "postgres") {
		t.Errorf("nil executor still added the postgres step: %v", names)
	}
}
