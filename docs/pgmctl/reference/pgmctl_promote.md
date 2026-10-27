## pgmctl promote

Promote NODE out of a join window whose publisher is lost (abandons writes)

### Synopsis

End a join window whose publisher is lost, by promoting NODE.

A join window is the state where the cluster's failover record lets only
its publisher promote: every other peer was still cloning when it was
published, so only the publisher holds every write it acknowledged. If
the publisher is gone for good, no survivor ever promotes on its own.

This command abandons every write the publisher acknowledged that NODE
has not received. It:

  1. reads the join window from the cluster and prints what is lost;
  2. asks for the typed cluster name (or --force --cluster NAME);
  3. records the authorization (POST /v1/promotion-authorization) on the
     node it talks to; any live node works, no leader is needed;
  4. waits until NODE is primary, unless --no-wait.

pg-manager refuses unless the publisher has been silent for its policy's
abandon window (60s by default), NODE holds the publisher's database,
and no live peer is ahead of NODE. It fences the publisher, so it cannot
promote when it returns; its abandoned writes can be inspected first.
Unfence it to let it re-clone.

Outside a join window, use 'pgmctl switchover --target NODE' or
'pgmctl failover'.

```
pgmctl promote NODE --accept-data-loss --reason TEXT [flags]
```

### Examples

```
  pgmctl promote node-a --accept-data-loss --reason "node-c volume lost, INC-1234"
  pgmctl promote --cancel
```

### Options

```
      --accept-data-loss   Required: accept losing the publisher's unreplicated writes
      --cancel             Cancel the pending authorization (the publisher stays fenced)
  -h, --help               help for promote
      --no-wait            Return once the authorization is recorded
      --reason string      Required: why the publisher is abandoned (recorded)
      --replace            Replace a pending authorization
      --ttl duration       How long the authorization may wait for NODE to promote (default: the cluster policy, 2m)
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

