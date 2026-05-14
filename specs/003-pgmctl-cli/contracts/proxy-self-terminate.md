# Contract — Proxy Peer Self-Terminate

**Feature**: `003-pgmctl-cli` · **Phase**: 1 · **Date**: 2026-05-14
**Source**: spec.md FR-031b / FR-031c, RD-009.

This contract pins the supervisor-presence heuristic and the
`POST /v1/restart` `target=proxy` handler behaviour.

---

## Supervisor presence detection

Run **once at proxy startup** in this order; first match wins.

| Order | Signal | Conclusion |
|---|---|---|
| 1 | `/.dockerenv` exists OR `/proc/1/cgroup` matches one of these regexes:<br>`/kubepods`<br>`/docker/`<br>`/containerd/`<br>`/system.slice/docker-`<br>`/system.slice/containerd-` | `container` |
| 2 | `INVOCATION_ID` env var set AND `JOURNAL_STREAM` env var set | `systemd` |
| 3 | `S6_OVERLAY_VERSION` env var set OR parent process basename ∈ {`s6-supervise`, `runsv`, `runsvdir`} | `s6_runit` |
| 4 | Parent process basename ∈ {`tini`, `dumb-init`} | `tini` |
| 5 | Config key `proxy.assume_supervised: true` | `override` |
| 6 | (default) | `none` |

The result is stored in the proxy's runtime state as
`SupervisorPresence` (per `data-model.md`).

### Implementation notes

- Cgroup probe reads `/proc/1/cgroup` once; parses each line; matches
  against the regexes above. Failures (missing file, parse error) are
  silently treated as no-match.
- Parent process basename comes from `/proc/<getppid()>/comm`. Failure
  to read silently treated as no-match.
- The startup log line `proxy.supervisor_presence_detected` records
  the result with the matching signal (e.g.,
  `{"presence":"systemd","signal":"env:INVOCATION_ID+JOURNAL_STREAM"}`)
  so operators can verify at startup.

---

## Handler behaviour

`POST /v1/restart` with `{ "target": "proxy", "target_node": "<self>" }`:

1. **Pre-auth** (handled by 001 bearer middleware).
2. **Audit prerequisite** (001 FR-028 — `audit_unavailable` ⇒ refuse).
3. **Peer match check**: if `target_node != receiving_peer.node_id`,
   return `400 invalid_argument` with `error.code = "wrong_peer"`. The
   receiving peer MUST be the one to self-terminate; the client
   resolves the right peer (via `Status` lookup + `--endpoint`) before
   sending.
4. **Supervisor check**: if `SupervisorPresence == "none"`, return
   `412 Precondition Failed` with:

   ```jsonc
   {
     "operation": "Restart",
     "request_id": "01H...XYZ",
     "outcome": "rejected",
     "error": {
       "code": "supervisor_not_detected",
       "message": "this pgman-proxy peer was not started under a detected supervisor (systemd, s6, tini, docker, kubernetes). Restart with --target=proxy is fail-closed unless the supervisor is detectable or `proxy.assume_supervised: true` is set."
     }
   }
   ```

   No drain. No exit. The peer keeps running.
5. **Audit emit**: write the audit record (`operation: Restart`,
   `outcome: accepted`, `target: target_node`, `extras: { target:
   "proxy" }`) to both the LCM audit subject and the history audit
   subject.
6. **Response emit**: write the standard LCM envelope to the HTTP
   response with `outcome: "accepted"`, flush, and close the response.
7. **Lifecycle event**: emit
   `proxy.self_restart_initiated { reason: "operator_restart",
   request_id }` to the structured-log sink and to the history event
   stream.
8. **Drain**: invoke the proxy's normal shutdown path (data-plane
   listener stops accepting; in-flight queries drain under the 001
   FR-014 budget; embedded NATS stops cleanly per 002 FR-012).
9. **Exit**: call `os.Exit(0)` with a clean shutdown code.
10. **Supervisor respawns** the process; pgmctl re-discovers the peer
    via its next request.

The HTTP response in step 6 is the operator's last contact with the
doomed peer. Any TCP error after that point is expected — pgmctl
treats post-202-response read errors as success, not failure.

---

## Failure modes

| Case | Behaviour |
|---|---|
| Drain budget exceeded | Proxy still exits, but emits `proxy.self_restart_drain_timeout` with the count of in-flight connections. |
| Audit pipeline unavailable | Step 2 refuses with `audit_unavailable` (001 FR-028) before any drain. |
| Bearer-auth missing / invalid | Standard 401 / 403 from the auth middleware. |
| `target_node != self` | `400 wrong_peer`. pgmctl is supposed to resolve the right peer first; this is mostly a developer-error guard. |
| Supervisor present but respawn fails (e.g., systemd unit disabled) | Out of pgmctl's scope — the peer is gone; the operator deals with their supervisor. |
| Proxy hangs in drain | The supervisor's standard kill-after-timeout handles this (`systemd TimeoutStopSec`, k8s `terminationGracePeriodSeconds`). pgman-proxy's own drain budget is shorter than typical supervisor budgets so the supervisor never needs to escalate. |

---

## Config keys added

| Key | Default | Notes |
|---|---|---|
| `proxy.assume_supervised` | `false` | Forces `SupervisorPresence = "override"`. Documented for the long tail of supervisors not in the detection list (custom init systems, embedded). |

A startup log line states the resolved `SupervisorPresence`. If
`override` is in effect, the log line includes a `WARN` level so the
operator sees they have opted into supervisor-trust at their own risk.

---

## Tests

Contract test (`tests/contract/restart_test.go`):

1. **`target=postgres` happy path**: start fixture cluster; call
   `POST /v1/restart` with `target=postgres`, `target_node=node-2`;
   assert response is `200 accepted`; assert `Manager.RestartPostgres`
   was invoked exactly once; assert the history stream contains a
   `state_transition` event reflecting the restart.

2. **`target=proxy` with supervisor**: start fixture peer under
   `tini`; call `POST /v1/restart` with `target=proxy`,
   `target_node=self`; assert response is `200 accepted`; assert the
   peer exits cleanly; assert tini respawns it; assert the post-restart
   peer accepts new requests within budget.

3. **`target=proxy` without supervisor**: start fixture peer as a
   bare process (no tini, no systemd env vars, no container probe);
   call `POST /v1/restart` with `target=proxy`; assert response is
   `412 supervisor_not_detected`; assert the peer is still running.

4. **`target=proxy` with override**: start fixture peer as a bare
   process but with `proxy.assume_supervised: true`; assert the
   restart proceeds (the test harness becomes the "supervisor" by
   re-spawning the process after observing exit).

5. **`target=proxy` wrong peer**: call `POST /v1/restart` against peer
   A with `target_node=peer-B`; assert `400 wrong_peer`; assert peer A
   is still running.

6. **`target=proxy` audit-unavailable**: simulate audit-pipeline
   outage; assert `503 audit_unavailable` per 001 FR-028; assert the
   peer is still running and no drain has begun.
