## pgmctl config view

Render the active configuration with secrets redacted

### Synopsis

Print the configuration file as YAML. Secret source references
(token files, token commands, env var names) are shown verbatim because
they are NOT the secret values themselves. To intentionally surface the
actual token contents, pass --show-secrets — refused in non-TTY contexts.

```
pgmctl config view [flags]
```

### Options

```
  -h, --help           help for view
      --show-secrets   Reveal token values inline (refused in non-TTY contexts)
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

* [pgmctl config](pgmctl_config.md)	 - Manage the local pgmctl configuration file (contexts, endpoints, credentials)

