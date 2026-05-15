## pgmctl explain

Compose a plain-English narrative from cluster facts

### Synopsis

pgmctl explain <subject> renders a three-section narrative
(Diagnosis / Evidence / Suggested next steps) by composing the existing
data layers: GET /v1/status, GET /v1/diagnose, POST /v1/doctor/run, and
GET /v1/history.

Subjects (FR-018):
  failover-stuck                  Why isn't a recently-elected leader being
                                  recognised by the rest of the cluster?
  node-not-promoting <node>       Why hasn't <node> been promoted yet?
  replication-broken <node>       Why is <node> not streaming WAL?
  leader-election                 Recent leader-election history.
  current-state                   One-line cluster shape rollup.
  last-event                      Most recent history record + its context.

Exit codes:
  0  narrative rendered
  4  EX_SUBJECT_NA — subject's premise doesn't match cluster state

```
pgmctl explain <subject> [<arg>] [flags]
```

### Options

```
  -h, --help   help for explain
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

