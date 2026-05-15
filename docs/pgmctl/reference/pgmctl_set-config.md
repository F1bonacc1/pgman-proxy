## pgmctl set-config

Trigger an in-process reload of a hot-reload-allow-listed key

### Synopsis

Re-reads the on-disk YAML and applies allow-listed changes.
The allow-list is intentionally narrow: cluster.route_peers, cluster.password.
Operators stage the change (YAML or secret rotation) and call this to apply.

Mirrors POST /v1/config/set; rejects disallowed keys with HTTP 400
set_config_key_disallowed.

```
pgmctl set-config --key <key> [flags]
```

### Options

```
  -h, --help         help for set-config
      --key string   Allow-listed config key (cluster.route_peers | cluster.password)
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

