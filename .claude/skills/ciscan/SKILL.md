---
name: ciscan
description: "CI fleet assessment — wide-first signal discovery and correlation"
---

# CI Fleet Assessment

Discover, correlate, and prioritize CI failure signals across environments. You are the wide net — you find what's worth investigating and produce a fleet assessment.

Your tool is `survey`, which provides three layers of signal:
1. **Signatures** — pre-grouped error patterns (what's failing)
2. **Envelopes** — per-run structural data from GCS artifacts (why it's failing)
3. **Fleet metrics** — pass rates, deploy correlation, regional/temporal patterns (where and when)

## Arguments

```
/ciscan [SCOPE] [DAYS]
```

- **SCOPE** — `periodic` (default), `presubmit`, or `all`
  - `periodic` — INT, STG, PROD environments. Production health, deploy correlation.
  - `presubmit` — Ephemeral PR test environments. Developer velocity, pipeline/build blockers.
  - `all` — Both. Use when the user says "everything" or "how is CI?"
- **DAYS** — Lookback period, max 7 (default: 7 for periodic, 5 for presubmit)

If the user doesn't specify a scope, ask: "Periodic (production health) or presubmit (PR velocity)?" — don't assume `all`.

## Routing

1. Have a specific run ID or Prow URL → `/cidig`
2. Need per-test Azure API traces or ARM operation timing → `/cidig`
3. Everything else ("how is CI?", "fleet status", "what's broken?") → `/ciscan` (you are here)

## Setup

```bash
go build -o /tmp/citriage ./tooling/citriage/
```

All output is JSON. GCS artifacts are cached locally — first scan fetches from remote, repeat scans are sub-second.

## Workflow

### Step 1: Gather fleet data

**periodic scope:**
```bash
/tmp/citriage survey --env=all --days=7
/tmp/citriage survey --env=int --job=nightly --days=7
/tmp/citriage survey --env=prod --job=nightly --days=7
```
Run all three in parallel.

**presubmit scope:**
```bash
/tmp/citriage survey --env=dev --days=5
```

**all scope:** Run periodic, nightly, and presubmit surveys in parallel.

### Step 2: Check data reliability

- **`data_window.truncated`** — if true, Sippy returned less data than requested. State the actual window and do not present pass rates as representative.
- **`ev2_coverage`** — if `with_ev2 / total < 50%`, deploy correlation is unreliable. Note it.

### Step 3: Read signatures — the pre-grouped problems

The `signatures` array groups failures by normalized error pattern. Each signature is one distinct problem — different tests that fail the same way appear in one signature.

Read signatures first: `key` is the normalized error, `hit_count` is total failures, `test_count` is how many distinct tests, `tests` lists them. `representative_error` has the full (unnormalized) error text from one instance.

For pipeline step failures, signatures use `extracted_errors` (ERROR/FATAL lines with ARM error codes) rather than the raw log output. The `key` for a pipeline step will show the actual ARM failure, not "Running step."

**`cross_env_signatures`** (in `--env=all`) groups signatures that appear in multiple environments. Same error pattern in STG and PROD = shared root cause. Different patterns = investigate separately.

The `failures` array still exists with per-test detail — use it when you need to look up a specific test's `best_run_id` or per-run outputs.

### Step 4: Read envelopes — the structural picture

Every failed run in `runs[]` has an `envelope` with data extracted from GCS artifacts. This tells you HOW and WHY the run failed, not just WHAT error text it produced.

**Read envelopes across all failing runs and compare:**

| Field | What it tells you | What to look for |
|---|---|---|
| `exit_code` | Pod-level outcome | 1 = test failure, 137 = OOM kill |
| `oom` | Memory kill | `true` = investigate memory, not test logic |
| `error_chain` | Compact failure summary from ci-operator | The full pod failure chain in ~500 bytes |
| `lease_wait_s` | Boskos lease acquisition | >60s = contention, >300s = severe |
| `pod_sched_s` | Pod scheduling latency | >30s = cluster pressure |
| `steps[]` | CI pipeline step results | Which step failed, how long, error snippet |
| `build_log_errors` | ERROR/FATAL lines from build log | ARM errors, resource contention |
| `build_log_steps` | Step SUCCEEDED/FAILED lines | Step durations, which passed vs failed |
| `alerts[]` | Azure Monitor alerts (presubmit) | KubeVersionMismatch, BackendControllerRetryHotLoop |
| `provision_failures[]` | Pipeline step failures (presubmit) | ARM deployment errors with step name |

**Cross-run patterns from envelopes:**
- All runs same `error_chain` → shared mechanism
- All runs `lease_wait_s < 2` and `pod_sched_s = 0` → infra is fine, failure is in tests/services
- `oom: true` on any run → memory issue, different investigation path
- `steps[].failed` consistent across runs → same CI step failing
- `provision_failures` with same ARM error code across PRs → infra issue, not PR code

### Step 5: Correlate findings

For each signature, correlate using fleet metrics AND envelopes:

- **Cross-env** — check `cross_env_signatures`. Multi-env = code/infra. Single-env = environment-specific.
- **Deploy correlation** — check `ev2_hash_rates`. All hashes failing = not deploy-correlated. One hash at 0% = suspect deploy.
- **Region** — check `region_rates`. Skewed pass rate = region-scoped.
- **Trend** — check `daily_rates`. Getting worse, improving, or flat?
- **Infra vs test** — check envelopes: `exit_code`, `oom`, `lease_wait_s`, `pod_sched_s`. If all normal, the failure is in the test/service layer, not CI infrastructure.

**For presubmit:**
- **PR concentration** — check `pull_number` on failing runs. Spread across many PRs = infra. Concentrated = PR-specific.
- **Provision failures** — check `runs[].envelope.provision_failures`. Same ARM error across PRs = infra provisioning issue. Highest urgency — blocks all PR testing.
- **Alerts** — check `runs[].envelope.alerts`. Alerts firing across runs = cluster health issue.

### Step 6: Prioritize and report

Order findings by impact. Consider: hit count, environment count, presubmit blocking, infrastructure vs test-level.

**Every failure must appear in the report.** Cluster by signature, list ALL tests per finding, prefix with `[PROD]`, `[STG]`, `[INT]`, or `[PRESUBMIT]`.

**Evidence from envelopes is REQUIRED.** For each finding, cite the structural evidence:
- "All 9 PROD runs: exit_code=1, oom=false, lease_wait <2s — infra healthy"
- "error_chain: ContainerFailed exit code 1 — test failure, not crash"
- "steps[]: prod-e2e-parallel at 7200s (full timeout) — tests ran to ceiling"

**When to launch `/cidig`:** Only when you need per-test depth that envelopes don't provide:
- Azure API trace for a specific test (which Azure call was slow?)
- ARM operation timing tree (which deployment step took 25 minutes?)
- Test output context for "Interrupted by User" errors (what was the test doing?)

Most fleet assessments should NOT need cidig. The envelope IS the investigation.

## Signal Absence as Evidence

What you DON'T see is diagnostic:
- No `cross_env_signatures` for an error → environment-specific
- `ev2_hash_rates` ALL hashes failing → not deploy-correlated
- All envelopes show `lease_wait_s < 2` → no Boskos contention
- All envelopes show `oom: false` → no memory issues
- `region_rates` uniform → not region-specific
- `failure_scale_dist` mostly "none" → isolated regressions, not systemic
- `provision_failures` empty on presubmit → provisioning succeeded, failure is in E2E tests

## Cross-Run-Type Correlation

Patterns that span run types reveal root cause layer:
- **Periodic timeout + presubmit provision failure** → shared infra
- **Same signature in periodic + presubmit** → code bug, not environment
- **Periodic fails, presubmit passes** → environment or deploy issue
- **Presubmit fails, periodic passes** → PR code issue or ephemeral infra
- **Nightly fails with different tests** → OCP candidate regression, not ARO

## Command Budget

**periodic:** survey --env=all (1) + nightly INT (1) + nightly PROD (1) = **3 max**
**presubmit:** survey --env=dev (1) = **1**
**all:** Both = **4 max**

## Data Reference

### Per-environment data (`survey --env=all`)

- **status**: `{streak, current_green, pass_rate, total_runs}`
- **data_window**: `{requested_days, actual_days, truncated}`
- **daily_rates**: `[{date, pass, total}]`
- **ev2_coverage**: `{with_ev2, total}`
- **ev2_hash_rates**: `[{hash, pass, fail, total, pass_rate, is_cron}]`
- **failure_scale_dist**: `{none, isolated, moderate, cascade}`
- **region_rates**: `[{region, pass, total, pass_rate, low_sample}]`
- **runs**: FAILED RUNS ONLY. Each run has:
  - `id, timestamp, overall_result, real_failures, ev2_hash, region, cluster, pull_number, sha, url`
  - `envelope`: `{exit_code, oom, error_chain, lease_wait_s, pod_sched_s, steps[], build_log_errors[], build_log_steps[], alerts[], provision_failures[]}`
  - `envelope.steps[]`: `{name, duration_seconds, failed, error_snippet}`
  - `envelope.provision_failures[]`: `{name, time_seconds, message}` (presubmit only)
  - `envelope.alerts[]`: `{name, severity, state}` (presubmit only)
- **failures**: `[{test_name, failure_count, first_failure, last_failure, last_pass, best_run_id, outputs[{run_id, error, extracted_errors}]}]`
  - Sorted by frequency desc. Outputs use signature-based deduplication — cascade failures share slots.
  - `extracted_errors` contains ERROR/FATAL lines for pipeline steps. Prefer over `error` for pipeline step diagnosis.
- **signatures**: `[{key, hit_count, test_count, tests[], first_failure, last_failure, best_run_id, representative_error}]`
  - Pre-grouped by normalized error. One signature = one problem regardless of test count.
  - Sorted by hit_count desc.
- **cross_env_failures**: `[{test_name, env_count, environments[{env, hits, run_id}]}]`
- **cross_env_signatures**: `[{key, env_count, total_hits, environments[{env, hits, run_id}]}]`

## Domain Knowledge

**Env mapping:** int=aro-integration, stg=aro-stage, prod=aro-production

**Three CI run types:**

| Type | Job pattern | Trigger | Query |
|---|---|---|---|
| Presubmit (ephemeral) | `pull-ci-*-e2e-parallel` | PR push | `--env=dev` |
| Presubmit (shared env) | `pull-ci-*-{stage,integration,prod}-e2e-parallel` | `/test` command | `--env=dev --job=stage` |
| Periodic | `periodic-ci-*-e2e-parallel` | EV2 deploy / cron | `--env=int/stg/prod` |

**EV2 annotations:** `ev2.rollout/ARO-HCP` = deploy hash, `ev2.rollout/region` = target region. Periodic only.

**Nightly runs:** Separate section. High expected failure rate (candidate OCP). Query with `--job=nightly`. Note nightly-specific failures.

## Output

### Step 7: Publish HTML report

**Template:** `tooling/citriage/report-template.html`. Do NOT read or copy the template — just write the DATA JSON and run the injection command.

**How:**
1. Write DATA object (JSON) to `/tmp/ciscan-data.json`
2. Inject into template: `python3 -c "import json; d=json.dumps(json.load(open('/tmp/ciscan-data.json')),ensure_ascii=False); t=open('tooling/citriage/report-template.html').read(); open('/tmp/ciscan-report.html','w').write(t.replace('const DATA = null;','const DATA = '+d+';'))"` (do NOT use sed — it mangles unicode escapes)
3. Open with `xdg-open /tmp/ciscan-report.html`

**DATA shape:**
```json
{
  "title": "CI Fleet Assessment",
  "subtitle": "scope · window · N findings",
  "generated": "ISO 8601",
  "fleetCards": [{ "env", "name", "passRate", "detail", "severity", "sparkline" }],
  "findings": [{
    "priority": "P1|P2|P3",
    "envs": ["int","stg"],
    "title": "...", "error": "...",
    "dims": {"Trend": "...", "Tests affected": "..."},
    "hypothesis": "...", "action": "...",
    "deployTimeline": [...], "affectedPRs": [...],
    "relatedFailures": [{"count", "name", "error", "url"}],
    "evidence": ["[source:field] observation — value"]
  }],
  "nightly": [{"env", "passRate", "runs", "detail"}],
  "coverageGaps": ["..."]
}
```

**Severity:** passRate <30% = "critical", 30-60% = "warning", >60% = "ok".

**Deploy timeline rows:** Each row is ONE RUN — `ts`, `hash`, `pass`/`fail`, `meta`. Set `newDeploy: true` on hash transitions.

**Findings evidence MUST cite envelope data.** Example: `"[envelope:error_chain] ContainerFailed exit code 1 — all 9 runs"`, `"[envelope:lease_wait_s] max 1.1s across all runs — no contention"`.

**In conversation:** Brief summary (fleet status, finding count). HTML report is the deliverable.

## Error Handling

| Situation | Action |
|---|---|
| Sippy down | Note "unavailable." Report what you have. |
| `data_window.truncated` | State actual window. Don't present as full baseline. |
| No failures | Fleet healthy. Report. |
| EV2 coverage < 50% | Note — deploy correlation unreliable. |
| Envelope missing on a run | Note — GCS artifacts unavailable for that run. |
