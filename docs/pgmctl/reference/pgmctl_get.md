## pgmctl get

Get a resource (nodes, peers, slots, topology, version, events, audit, config)

### Synopsis

Fetch a single resource kind. Use one of:

  nodes, peers     — peer table from /v1/status
  slots            — replication slots from /v1/diagnose
  topology         — cluster topology tree (derived from /v1/status)
  version          — client + server versions (uses /v1/version when present)
  events, audit    — DEFERRED (added in feature 003 Phase 4 — history stream)
  config           — DEFERRED (server-side GET /v1/config not yet implemented)


```
pgmctl get <resource> [<name>] [flags]
```

### Options

```
  -h, --help   help for get
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

