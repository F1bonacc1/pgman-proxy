## pgmctl

Operator CLI for pgman-proxy clusters

### Synopsis

pgmctl is the operator CLI for a running pgman-proxy cluster.
It consumes the existing HTTP control-plane and embedded-NATS observability
surfaces and renders them with kubectl-style ergonomics.

Spec: specs/003-pgmctl-cli/spec.md


### Options

```
      --cluster string                Expected cluster id; pinned before any cluster-affecting op
      --config string                 Path to pgmctl config (overrides $XDG_CONFIG_HOME/pgmctl/config.yaml)
      --context string                Configured context name
      --endpoint string               Single-shot pgman-proxy endpoint override
      --force                         Skip cluster-name confirmation (requires --cluster <name>)
  -h, --help                          help for pgmctl
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
* [pgmctl describe](pgmctl_describe.md)	 - Verbose form of `get` — emits the full record set
* [pgmctl doctor](pgmctl_doctor.md)	 - Run cluster health checks and render findings
* [pgmctl dump](pgmctl_dump.md)	 - Capture every slice into a single tar.gz / tar artifact
* [pgmctl events](pgmctl_events.md)	 - Tail the cluster's event history
* [pgmctl explain](pgmctl_explain.md)	 - Compose a plain-English narrative from cluster facts
* [pgmctl failover](pgmctl_failover.md)	 - Trigger an unplanned failover of the current primary
* [pgmctl fence](pgmctl_fence.md)	 - Add a node to the cluster fence list
* [pgmctl get](pgmctl_get.md)	 - Get a resource (nodes, peers, slots, topology, version, events, audit, config)
* [pgmctl health](pgmctl_health.md)	 - One-line-per-component health rollup
* [pgmctl lag](pgmctl_lag.md)	 - Per-standby replication lag in bytes
* [pgmctl list](pgmctl_list.md)	 - List a resource (alias of `get` for collection-shaped resources)
* [pgmctl promote](pgmctl_promote.md)	 - Manually promote THIS peer (local-only override)
* [pgmctl restart](pgmctl_restart.md)	 - Restart a peer's PostgreSQL or the pgman-proxy process itself
* [pgmctl set-config](pgmctl_set-config.md)	 - Trigger an in-process reload of a hot-reload-allow-listed key
* [pgmctl status](pgmctl_status.md)	 - One-glance cluster health
* [pgmctl switchover](pgmctl_switchover.md)	 - Gracefully promote a specific peer
* [pgmctl topology](pgmctl_topology.md)	 - Render the cluster topology (peers, roles, sync standbys)
* [pgmctl unfence](pgmctl_unfence.md)	 - Remove a node from the cluster fence list
* [pgmctl version](pgmctl_version.md)	 - Print pgmctl + server versions and report skew
* [pgmctl watch](pgmctl_watch.md)	 - Live views of cluster state via Server-Sent Events

