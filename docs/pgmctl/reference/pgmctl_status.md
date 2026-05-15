## pgmctl status

One-glance cluster health

### Synopsis

Render a compact summary of the connected cluster's health:
cluster id, leader, primary, peer count, embedded-NATS mesh state, per-peer
role/fence/lag/last-transition, and the time-of-snapshot.

Exit codes:
  0   healthy
  2   unhealthy (no primary / no leader / any failed peer)
  65  unreachable (connection / TLS / auth failure)


```
pgmctl status [flags]
```

### Options

```
  -h, --help   help for status
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

