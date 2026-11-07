// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package runtime

import (
	"fmt"
	"os"

	"github.com/f1bonacc1/pgman-proxy/internal/config"
)

// checkPostgresTLSFiles fails startup when a configured peer-TLS file
// (postgres.tls_ca_file, tls_cert_file, tls_key_file) cannot be read,
// naming the setting, instead of letting every replication connection
// fail later with a libpq error. Files are ignored under tls_mode=disable,
// which never uses them.
func checkPostgresTLSFiles(p config.PostgresConfig) error {
	if p.EffectiveTLSMode() == config.TLSModeDisable {
		return nil
	}
	for _, f := range []struct{ key, path string }{
		{"postgres.tls_ca_file", p.TLSCAFile},
		{"postgres.tls_cert_file", p.TLSCertFile},
		{"postgres.tls_key_file", p.TLSKeyFile},
	} {
		if f.path == "" {
			continue
		}
		fh, err := os.Open(f.path)
		if err != nil {
			return fmt.Errorf("%s: %w", f.key, err)
		}
		_ = fh.Close()
	}
	return nil
}
