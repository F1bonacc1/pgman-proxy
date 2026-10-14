// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

package scaleout

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Paths inside the integration image. The data and store directories
// are separate named volumes: the postgres base image declares a
// VOLUME on its data dir, which conflicts with a single parent mount.
const (
	dataDir  = "/var/lib/postgresql/data"
	storeDir = "/var/lib/postgresql/js"
	binDir   = "/usr/lib/postgresql/17/bin"
)

// docker runs `docker <args...>` and returns combined output.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// topo is one scenario's private docker world: a network plus the
// containers and volumes created through it, all named with a unique
// prefix so scenarios never collide and cleanup is a prefix sweep.
type topo struct {
	t      *testing.T
	prefix string
	net    string
	// history holds the logs of containers removed by a restart into
	// another config, keyed by node, oldest first.
	history map[string][]string
}

// newTopo creates the scenario's network and registers cleanup. On a
// failed test every container's logs are written to the test output
// first: the containers hold the only record of what the engine did.
func newTopo(t *testing.T, scenario string) *topo {
	t.Helper()
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	tp := &topo{t: t, prefix: "pgso-" + scenario + "-" + hex.EncodeToString(b), history: map[string][]string{}}
	tp.net = tp.prefix + "-net"
	tp.mustDocker(time.Minute, "network", "create", tp.net)
	t.Cleanup(tp.cleanup)
	return tp
}

func (tp *topo) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	names, _ := docker(ctx, "ps", "-a", "--filter", "name="+tp.prefix, "--format", "{{.Names}}")
	containers := strings.Fields(names)
	sort.Strings(containers)
	if tp.t.Failed() {
		nodes := make([]string, 0, len(tp.history))
		for n := range tp.history {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		for _, n := range nodes {
			for i, l := range tp.history[n] {
				tp.t.Logf("=== %s (previous run %d) logs ===\n%s", tp.container(n), i+1, l)
			}
		}
		for _, c := range containers {
			state, _ := docker(ctx, "inspect", "-f", "{{.State.Status}} exit={{.State.ExitCode}}", c)
			logs, _ := docker(ctx, "logs", "--timestamps", c)
			tp.t.Logf("=== %s (%s) logs ===\n%s", c, strings.TrimSpace(state), logs)
		}
	}
	if os.Getenv("PGMAN_SCALEOUT_KEEP") == "1" {
		tp.t.Logf("PGMAN_SCALEOUT_KEEP=1: leaving containers, volumes and network %s* in place", tp.prefix)
		return
	}
	if len(containers) > 0 {
		_, _ = docker(ctx, append([]string{"rm", "-f"}, containers...)...)
	}
	vols, _ := docker(ctx, "volume", "ls", "-q", "--filter", "name="+tp.prefix)
	if v := strings.Fields(vols); len(v) > 0 {
		_, _ = docker(ctx, append([]string{"volume", "rm", "-f"}, v...)...)
	}
	_, _ = docker(ctx, "network", "rm", tp.net)
}

func (tp *topo) mustDocker(timeout time.Duration, args ...string) string {
	tp.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := docker(ctx, args...)
	if err != nil {
		tp.t.Fatal(err)
	}
	return out
}

func (tp *topo) container(node string) string { return tp.prefix + "-node-" + node }

func (tp *topo) volume(node, kind string) string { return tp.prefix + "-" + node + "-" + kind }

// rootRun runs a one-shot root container from the test image with the
// given volume mounts (volume name → mount path) and shell script.
func (tp *topo) rootRun(mounts map[string]string, script string) string {
	tp.t.Helper()
	args := []string{"run", "--rm", "-u", "root", "--entrypoint", "sh"}
	for vol, path := range mounts {
		args = append(args, "-v", vol+":"+path)
	}
	args = append(args, image, "-c", script)
	return tp.mustDocker(5*time.Minute, args...)
}

// ensureVolumes creates node's data and store volumes owned by the
// postgres user. Idempotent: existing volumes keep their contents.
func (tp *topo) ensureVolumes(node string) {
	tp.t.Helper()
	for _, kind := range []string{"data", "js"} {
		v := tp.volume(node, kind)
		tp.mustDocker(time.Minute, "volume", "create", v)
		tp.rootRun(map[string]string{v: "/v"}, "chown postgres:postgres /v && chmod 0700 /v")
	}
}

// start runs node with env as a fresh container (env cannot change on
// an existing container, so a restart into another config is
// stop + remove + start). The network alias node-<x> lets peer ids
// resolve as DNS names, as in the compose topology.
func (tp *topo) start(node string, env map[string]string) {
	tp.t.Helper()
	tp.remove(node)
	args := []string{"run", "-d", "--name", tp.container(node),
		"--hostname", "node-" + node, "--network", tp.net, "--network-alias", "node-" + node,
		"-v", tp.volume(node, "data") + ":" + dataDir,
		"-v", tp.volume(node, "js") + ":" + storeDir}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, image)
	tp.mustDocker(2*time.Minute, args...)
}

// stop sends SIGTERM and waits up to timeout before SIGKILL. The
// container is kept so its logs stay readable.
func (tp *topo) stop(node string, timeout time.Duration) {
	tp.t.Helper()
	tp.mustDocker(timeout+time.Minute, "stop", "-t", strconv.Itoa(int(timeout.Seconds())), tp.container(node))
}

// remove deletes node's container if it exists, keeping its volumes.
// Logs of a removed container are appended to the test log first, so
// a restart into another config does not lose the earlier run's trail.
func (tp *topo) remove(node string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := docker(ctx, "inspect", tp.container(node)); err != nil {
		return
	}
	if logs, err := docker(ctx, "logs", "--timestamps", tp.container(node)); err == nil {
		tp.history[node] = append(tp.history[node], logs)
	}
	_, _ = docker(ctx, "rm", "-f", tp.container(node))
}

// logs returns the current container's log (stdout + stderr).
func (tp *topo) logs(node string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, _ := docker(ctx, "logs", tp.container(node))
	return out
}

// allLogs returns every run's log for node, oldest first, including
// containers removed by restarts into another config.
func (tp *topo) allLogs(node string) string {
	return strings.Join(append(append([]string{}, tp.history[node]...), tp.logs(node)), "\n")
}

// wait blocks until node's container exits and returns its exit code.
func (tp *topo) wait(node string, budget time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	out, err := docker(ctx, "wait", tp.container(node))
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// exec runs a command inside node's container as the postgres user.
func (tp *topo) exec(node string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return docker(ctx, append([]string{"exec", tp.container(node)}, args...)...)
}
