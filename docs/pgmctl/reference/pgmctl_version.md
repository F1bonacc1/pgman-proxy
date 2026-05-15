## pgmctl version

Print pgmctl + server versions and report skew

### Synopsis

Print the pgmctl client version, the server's pgman-proxy version
(when reachable), and any version-skew between them.

Skew rules:
  patch  → silent
  minor  → yellow warning
  major  → refuse with exit code 67 unless --insecure-skip-version-check.

When the server endpoint is unconfigured or unreachable, prints the
client version only and exits 0.

```
pgmctl version [flags]
```

### Options

```
  -h, --help   help for version
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

