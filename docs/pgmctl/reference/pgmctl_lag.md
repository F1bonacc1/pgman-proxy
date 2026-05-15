## pgmctl lag

Per-standby replication lag in bytes

### Synopsis

Show replication lag for every standby — bytes behind the
primary's flush LSN, with severity coloring based on thresholds.

Defaults: --warn 64MB, --fail 1GB.

```
pgmctl lag [flags]
```

### Options

```
      --fail string   Lag threshold for FAIL severity (e.g. 1GB, 2GiB) (default "1GB")
  -h, --help          help for lag
      --warn string   Lag threshold for WARN severity (e.g. 64MB, 128MiB) (default "64MB")
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

