## pgmctl events

Tail the cluster's event history

### Synopsis

Stream the cluster's event history from the JetStream-backed
history stream (feature 003 FR-016a) via GET /v1/history.

Defaults: --since 30m, --limit 1000.

```
pgmctl events [flags]
```

### Options

```
      --cursor string    Resume from after this event id (ULID)
  -h, --help             help for events
      --limit int        Maximum number of events to return (default 1000)
      --list-types       Show the distinct event types observed in the window (with counts + last-seen)
      --node strings     Filter to specific node ids (repeatable)
      --since duration   Only show events newer than this duration (e.g. 5m, 24h) (default 30m0s)
      --type strings     Filter to specific event types (repeatable)
      --until string     Only show events older than this RFC3339 timestamp
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

