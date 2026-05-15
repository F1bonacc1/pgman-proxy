## pgmctl dump

Capture every slice into a single tar.gz / tar artifact

### Synopsis

Captures cluster-wide state in parallel — status, topology, history
events + audit, doctor (when implemented), clock-skew, config — and writes
a single tar archive at --output. Use --output - to stream raw tar to
stdout (compression off).

Per-slice failures don't fail the dump: each slice is recorded in
manifest.json with an outcome (ok|failed). The command exits 0 when every
slice succeeded and 3 (EX_PARTIAL) when at least one slice failed (FR-037).

Exit codes:
  0   clean dump
  3   one or more slices failed (manifest documents the gap)
  124 outer command timeout

```
pgmctl dump [flags]
```

### Options

```
  -h, --help                         help for dump
      --output string                Output path; '-' streams raw tar to stdout
      --per-slice-timeout duration   Per-slice fetch timeout (default 10s)
      --redact-level string          Redaction level: normal|strict (default "normal")
      --since duration               Time window for history slices (e.g. 30m, 24h); empty = server default
```

### Options inherited from parent commands

```
      --cluster string                Expected cluster id; pinned before any cluster-affecting op
      --config string                 Path to pgmctl config (overrides $XDG_CONFIG_HOME/pgmctl/config.yaml)
      --context string                Configured context name
      --endpoint string               Single-shot pgman-proxy endpoint override
      --force                         Skip cluster-name confirmation (requires --cluster <name>)
      --insecure-skip-tls-verify      Disable TLS verification (warned)
      --insecure-skip-version-check   Proceed despite minor / major version skew
      --no-color                      Suppress ANSI escapes unconditionally
  -q, --quiet                         Suppress non-essential output
      --strict                        Treat WARN as non-zero exit
      --timeout duration              Overall command timeout (default 10s)
  -v, --verbose count                 Increase verbosity (-v / -vv / -vvv)
  -y, --yes                           Skip single-resource confirmation prompts
```

### SEE ALSO

* [pgmctl](pgmctl.md)	 - Operator CLI for pgman-proxy clusters

