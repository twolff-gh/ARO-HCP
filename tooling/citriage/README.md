# citriage

CI fleet triage tool for ARO-HCP. Fetches data from Sippy, GCS artifacts, CI Search, and GitHub, pre-computes fleet analysis (root cause clustering, co-failure detection, region breakdown, deploy correlation), and outputs structured JSON for LLM consumption.

Two Claude Code skills provide the interface:
- **`/ciscan`** — Fleet assessment: what's broken, since when, how broadly
- **`/cidig`** — Single-run investigation: root cause analysis from artifacts

## Build

```bash
go build -o /tmp/citriage ./tooling/citriage/
```

No external dependencies. Pure Go standard library.

## Commands

| Command | Purpose | Data source |
|---|---|---|
| `survey --env=all --days=7` | Fleet health, failures, root causes, co-failure groups, region rates | Sippy + GCS |
| `triage <run-id>` | Unified signal packet — all artifacts compressed into one JSON object | GCS + Sippy |
| `dig <run-id> <what>` | Per-run artifact drill-down (tests, azure, metrics, provision, etc.) | GCS |

All output is JSON.

### `triage` — unified signal packet

Extracts a compressed summary from every artifact type in a single call. Eliminates guessing which `dig` subcommand to try.

```bash
citriage triage <run-id> [--context-days=3] [--baseline=<passing-run-id>]
```

The signal packet contains:

| Field | Source | Size |
|---|---|---|
| `scale` | extension_test_result | ~150 bytes |
| `errors` | extension_test_result (deduplicated) | ~1-45KB |
| `steps` | ci-operator-step-graph.json | ~1KB |
| `metrics` | ci-operator-metrics.json | ~3KB |
| `build_log` | build-log.txt (ERROR lines, step lines, tail) | ~1KB |
| `podinfo` | podinfo.json (exit code, reason, OOM flag) | ~50 bytes |
| `events` | events.json (CiJobFailed, warning count) | ~40 bytes |
| `pool` | identities-pool-state.yaml (counts, contention) | ~80 bytes |
| `azure` | azure.log (ResponseError codes per test) | ~100 bytes/test |
| `provision` | junit_entrypoint.xml (failed steps, presubmit) | ~200 bytes |
| `alerts` | alerts.json (unique alerts, presubmit) | ~100 bytes |
| `timing` | timing-metadata YAML (slowest ARM op per test) | ~100 bytes/test |
| `links` | custom-link-tools HTML (RG names, Kusto links) | ~200 bytes |
| `neighbors` | Sippy (same EV2 hash pass/fail rates) | ~100 bytes |
| `coverage` | Artifact availability flags and key infrastructure signal values | ~200 bytes |

Budget-aware per-test extraction: azure, timing, and output_tails are included for runs with ≤5 failures, use representative samples for 6-20, and are skipped for cascades (21+). Total packet: ~12-24KB for isolated failures, ~14KB for cascades.

## Data sources

- **Sippy** — Run listings, test failure rates, durations, pass rates
- **GCS** — Per-run artifacts (junit, Azure logs, K8s events, metrics, pool state, pod info, timing metadata)
