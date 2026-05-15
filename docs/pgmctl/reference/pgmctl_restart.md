## pgmctl restart

Restart a peer's PostgreSQL or the pgman-proxy process itself

### Synopsis

Two modes:

  --target=postgres
    Restarts the LOCAL Postgres on the peer the request lands on. Use
    --endpoint to direct the request to a specific peer (or rely on
    the active context). Engine emits state-transition events; if the
    target is the current primary, a failover may follow.

  --target=proxy
    Restarts the pgman-proxy process itself on the target peer. The
    peer MUST be running under a process supervisor (tini / systemd /
    k8s / process-compose) that will bring it back; otherwise the
    server returns 412 supervisor_not_detected.

Cluster-affecting in both modes; requires typed cluster name (or
--force --cluster <name>).

```
pgmctl restart --target=<postgres|proxy> [--target-node <id>] [flags]
```

### Options

```
  -h, --help                 help for restart
      --target string        What to restart: postgres | proxy (default "postgres")
      --target-node string   Node id (required for --target=proxy)
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

