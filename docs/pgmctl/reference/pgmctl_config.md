## pgmctl config

Manage the local pgmctl configuration file (contexts, endpoints, credentials)

### Synopsis

Manage the pgmctl kubeconfig-style configuration at
$XDG_CONFIG_HOME/pgmctl/config.yaml (fallback ~/.config/pgmctl/config.yaml).

All operations are local file operations; nothing on the network is touched.

### Options

```
  -h, --help   help for config
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
* [pgmctl config delete-context](pgmctl_config_delete-context.md)	 - Remove a context
* [pgmctl config set-context](pgmctl_config_set-context.md)	 - Create or update a named context
* [pgmctl config use-context](pgmctl_config_use-context.md)	 - Set the current-context to <name>
* [pgmctl config view](pgmctl_config_view.md)	 - Render the active configuration with secrets redacted

