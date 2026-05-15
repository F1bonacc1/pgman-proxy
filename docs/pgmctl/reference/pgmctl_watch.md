## pgmctl watch

Live views of cluster state via Server-Sent Events

### Synopsis

Subscribe to the cluster control plane's /v1/watch/* SSE endpoints
and render a live view that updates as state changes.

Subcommands:
  status        Fixed-line cluster summary, redrawn on every change.
  transitions   Append-only state-transition log.
  events        Append-only history-event log (filterable).
  node <id>     Append-only stream of one node's events.

Watch streams reconnect automatically with exponential backoff. A
gap_marker line indicates the stream may have missed events.

For machine-readable consumption use 'pgmctl events --since 0' instead
of redirecting watch output; -o json/yaml is rejected here.

### Options

```
  -h, --help   help for watch
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
* [pgmctl watch events](pgmctl_watch_events.md)	 - Live history-event stream (filterable)
* [pgmctl watch node](pgmctl_watch_node.md)	 - Live event stream filtered to a single node
* [pgmctl watch status](pgmctl_watch_status.md)	 - Live cluster status (fixed-line redraw)
* [pgmctl watch transitions](pgmctl_watch_transitions.md)	 - Live state-transition stream

