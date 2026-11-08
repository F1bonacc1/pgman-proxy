// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package conninfo

import (
	"strings"
	"testing"
)

// basebackupForm is a standby's primary_conninfo exactly as the
// 2026-11-08 nightly read it: written by pg_basebackup -R, which
// expands every libpq default and single-quotes any value holding a
// character outside [A-Za-z0-9_].
const basebackupForm = `user=postgres passfile='/var/lib/postgresql/.pgpass' channel_binding=prefer host='node-b' port=5432 application_name='node-a' sslmode='verify-full' sslnegotiation=postgres sslcompression=0 sslcertmode=allow sslrootcert='/etc/pgman-tls/ca.crt' sslsni=1 ssl_min_protocol_version=TLSv1.2 gssencmode=prefer krbsrvname=postgres gssdelegation=0 target_session_attrs=any load_balance_hosts=disable dbname=postgres`

// renderedForm is the same target as pg-manager renders it when it
// rewrites primary_conninfo itself: the peer DSN, unquoted.
const renderedForm = `host=node-b port=5432 user=postgres dbname=postgres sslmode=verify-full sslrootcert=/etc/pgman-tls/ca.crt application_name=node-a`

func TestParse_BothWriterFormsAgree(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{"basebackup": basebackupForm, "rendered": renderedForm} {
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		for k, want := range map[string]string{
			"host":             "node-b",
			"port":             "5432",
			"user":             "postgres",
			"dbname":           "postgres",
			"sslmode":          "verify-full",
			"sslrootcert":      "/etc/pgman-tls/ca.crt",
			"application_name": "node-a",
		} {
			if got[k] != want {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], want)
			}
		}
	}
}

func TestParse_QuotingEscapesAndSpacing(t *testing.T) {
	t.Parallel()
	got, err := Parse(`password='it\'s a \\ secret'  host = node-a path=a\ b sslcert='' port=5432`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"password": `it's a \ secret`,
		"host":     "node-a",
		"path":     "a b",
		"sslcert":  "",
		"port":     "5432",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d keys %v, want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("%s = %q (present %v), want %q", k, gv, ok, v)
		}
	}
}

func TestParse_Malformed(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		`host='node-a`,     // unterminated quote
		`host node-a`,      // missing "="
		`=node-a`,          // missing keyword
		`host=node-a port`, // trailing keyword without value
	} {
		if got, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", s, got)
		} else if !strings.Contains(err.Error(), "conninfo") {
			t.Errorf("Parse(%q) error %q does not say conninfo", s, err)
		}
	}
}
