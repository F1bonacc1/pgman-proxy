// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

// Package conninfo parses libpq keyword/value connection strings for
// test assertions. A standby's primary_conninfo comes from one of two
// writers: pg_basebackup -R, which single-quotes values such as
// 'verify-full', or pg-manager's own renderer, which writes them bare.
// Substring checks see a difference that libpq does not, so tests must
// compare parsed values.
package conninfo

import (
	"fmt"
	"strings"
)

// Parse splits s into its keyword/value pairs, following libpq's
// keyword/value rules: pairs are separated by whitespace, spaces may
// surround "=", a value is bare or single-quoted, and a backslash
// escapes the next character in either form. A repeated keyword keeps
// its last value, as in libpq. URIs (postgresql://...) are not
// supported.
func Parse(s string) (map[string]string, error) {
	out := make(map[string]string)
	i, n := 0, len(s)
	skipSpace := func() {
		for i < n && isSpace(s[i]) {
			i++
		}
	}
	for {
		skipSpace()
		if i >= n {
			return out, nil
		}
		start := i
		for i < n && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		key := s[start:i]
		skipSpace()
		if key == "" || i >= n || s[i] != '=' {
			return nil, fmt.Errorf("conninfo: missing \"=\" after keyword %q at offset %d", key, start)
		}
		i++ // past "="
		skipSpace()
		var val strings.Builder
		if i < n && s[i] == '\'' {
			i++
			closed := false
			for i < n {
				c := s[i]
				if c == '\\' && i+1 < n {
					val.WriteByte(s[i+1])
					i += 2
					continue
				}
				if c == '\'' {
					closed = true
					i++
					break
				}
				val.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("conninfo: unterminated quoted value for %q", key)
			}
		} else {
			for i < n && !isSpace(s[i]) {
				if s[i] == '\\' && i+1 < n {
					i++
				}
				val.WriteByte(s[i])
				i++
			}
		}
		out[key] = val.String()
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}
