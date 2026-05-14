# pgmctl golden files

Convention: one file per `<command>_<scenario>.<format>`, where
`<format>` is one of `txt` (no-color table), `ansi` (table with ANSI
escapes preserved), `json`, `yaml`.

Examples:

```text
status_healthy.txt
status_healthy.ansi
status_healthy.json
status_healthy.yaml
status_warn.txt
status_fail.txt
doctor_pass.txt
doctor_one_fail.txt
dump_manifest_healthy.json
dump_manifest_partial.json
prompts_fence.txt
prompts_failover.txt
```

Update golden files via `go test ./tests/contract/pgmctl/ -update`.

Re-record only intentional output changes; an unintentional diff
against a golden file is the test telling you the user-visible output
just shifted.
