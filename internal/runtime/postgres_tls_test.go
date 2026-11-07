// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

func TestCheckPostgresTLSFiles(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPostgresTLSFiles(config.PostgresConfig{TLSCAFile: ca}); err != nil {
		t.Fatalf("readable CA: %v", err)
	}
	err := checkPostgresTLSFiles(config.PostgresConfig{TLSCAFile: filepath.Join(dir, "missing.pem")})
	if err == nil || !strings.Contains(err.Error(), "postgres.tls_ca_file") {
		t.Fatalf("missing CA: want error naming postgres.tls_ca_file, got %v", err)
	}
	if err := checkPostgresTLSFiles(config.PostgresConfig{TLSMode: "disable", TLSCAFile: "/nope"}); err != nil {
		t.Fatalf("tls_mode=disable never reads the files: %v", err)
	}
}
