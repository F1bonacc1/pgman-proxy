## pgmctl doctor

Run cluster health checks and render findings

### Synopsis

pgmctl doctor runs the server-published v1 check battery and renders
the results with severity coloring. Use --list to inspect the catalog
without running anything; --check <name> to run a single check.

Exit codes:
  0   every check PASS or INFO (and --strict not set)
  1   one or more WARN findings AND --strict
  2   one or more FAIL findings
  5   one or more UNKNOWN findings (and no FAIL)

```
pgmctl doctor [flags]
```

### Options

```
      --check string   Run a single check by name (omit to run all)
  -h, --help           help for doctor
      --list           List the registered checks; do not execute them
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
  -o, --output string                 Output format (table, json, yaml, wide) (default "table")
  -q, --quiet                         Suppress non-essential output
      --strict                        Treat WARN as non-zero exit
      --timeout duration              Overall command timeout (default 10s)
  -v, --verbose count                 Increase verbosity (-v / -vv / -vvv)
  -y, --yes                           Skip single-resource confirmation prompts
```

### SEE ALSO

* [pgmctl](pgmctl.md)	 - Operator CLI for pgman-proxy clusters

