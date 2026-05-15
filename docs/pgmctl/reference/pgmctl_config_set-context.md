## pgmctl config set-context

Create or update a named context

### Synopsis

Create a new context, or update the named context in place.

Credentials: exactly one of --token-env, --token-file, or --token-command
must be supplied. Plaintext tokens are NEVER accepted on the flag list —
that's a deliberate non-feature to keep secrets out of shell history.

```
pgmctl config set-context <name> [flags]
```

### Options

```
      --endpoint string            Control-plane URL (https://host:port)
      --expected-cluster string    Cluster id pinned to this context
  -h, --help                       help for set-context
      --insecure-skip-tls-verify   Disable TLS verification for this context
      --tls-ca-file string         TLS trust anchor PEM bundle
      --tls-server-name string     TLS SNI server name override
      --token-command strings      Command whose stdout is the bearer token (repeatable for argv parts)
      --token-env string           Env var name holding the bearer token
      --token-file string          File path holding the bearer token
```

### Options inherited from parent commands

```
      --cluster string                Expected cluster id; pinned before any cluster-affecting op
      --config string                 Path to pgmctl config (overrides $XDG_CONFIG_HOME/pgmctl/config.yaml)
      --context string                Configured context name
      --force                         Skip cluster-name confirmation (requires --cluster <name>)
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

