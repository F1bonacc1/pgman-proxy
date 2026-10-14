// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

//go:build scaleout

// Package scaleout drives real pgman-proxy nodes (embedded NATS +
// colocated PostgreSQL 17) with the docker CLI to exercise topology
// changes the compose-based tests/integration suite cannot: a single
// node restarted into a 3-node config, rollback to one node, and nodes
// held back on purpose. Scenarios are specified in
// specs/005-scale-out-1-to-3/contracts/scaleout-suite.md.
//
// `make scaleout` is the canonical entrypoint.
package scaleout

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// image is the docker image every scenario runs. Set once by TestMain.
var image string

// TestMain builds the integration image once per run (unless
// PGMAN_SCALEOUT_IMAGE names a prebuilt one) and removes it afterwards.
// Scenarios own their containers, volumes and networks via newTopo.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintf(os.Stderr, "scaleout: docker not on PATH: %v\n", err)
		os.Exit(2)
	}
	built := false
	image = os.Getenv("PGMAN_SCALEOUT_IMAGE")
	if image == "" {
		tag, err := buildImage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "scaleout: image build failed: %v\n", err)
			os.Exit(2)
		}
		image, built = tag, true
	}
	code := m.Run()
	if built && os.Getenv("PGMAN_SCALEOUT_KEEP") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, _ = docker(ctx, "rmi", "-f", image)
		cancel()
	}
	os.Exit(code)
}

// repoRoot returns the pgman-proxy checkout root (two levels above this
// package directory, which `go test` uses as the working directory).
func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Dir(filepath.Dir(wd)), nil
}

// buildImage builds tests/integration/Dockerfile exactly as the compose
// harness does: the build context is the parent of the checkout, so
// the Dockerfile can COPY both pg-manager (sibling) and pgman-proxy.
func buildImage() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	tag := fmt.Sprintf("pgman-proxy-scaleout:%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", //nolint:gosec
		"-f", filepath.Join(root, "tests", "integration", "Dockerfile"),
		"-t", tag, filepath.Dir(root))
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker build: %w\n%s", err, buf.String())
	}
	return tag, nil
}

// requireUpstream015 skips scenarios that need pg-manager milestone
// 015-empty-peer-safety (empty-node deference, empty-peer exclusion
// from the sync set). Research R-009: the image always builds against
// the sibling checkout, so PGMAN_SCALEOUT_UPSTREAM_015=1 lets a
// developer run them before go.mod pins a release that has 015.
func requireUpstream015(t *testing.T) {
	t.Helper()
	if !upstream015() {
		t.Skip("needs pg-manager 015-empty-peer-safety (set PGMAN_SCALEOUT_UPSTREAM_015=1 to run against a sibling checkout that has it)")
	}
}

// upstream015 reports whether the engine under test is known to carry
// milestone 015, for assertions that flip once it lands (S5b).
func upstream015() bool {
	if os.Getenv("PGMAN_SCALEOUT_UPSTREAM_015") == "1" {
		return true
	}
	root, err := repoRoot()
	if err != nil {
		return false
	}
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/f1bonacc1/pg-manager") //nolint:gosec
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return semverAtLeast(strings.TrimSpace(string(out)), 0, 7, 0)
}

// semverAtLeast compares a "vMAJOR.MINOR.PATCH[-pre][+meta]" version
// against major.minor.patch. Unparseable input compares as false.
func semverAtLeast(v string, major, minor, patch int) bool {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		n[i] = x
	}
	want := [3]int{major, minor, patch}
	for i := range n {
		if n[i] != want[i] {
			return n[i] > want[i]
		}
	}
	return true
}

func TestSemverAtLeast(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"v0.6.1", false},
		{"v0.7.0", true},
		{"v0.7.0-rc.1", true},
		{"v0.10.0", true},
		{"v1.0.0", true},
		{"", false},
		{"(devel)", false},
	}
	for _, c := range cases {
		if got := semverAtLeast(c.v, 0, 7, 0); got != c.want {
			t.Errorf("semverAtLeast(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
