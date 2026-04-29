---
name: ciscan
description: "CI fleet assessment — wide-first signal discovery and correlation"
---

# CI Fleet Assessment

Discover, correlate, and prioritize CI failure signals across environments. You are the diagnostician — you assess pre-computed structural evidence and produce a fleet assessment with hypotheses and recommendations.

Your tool is `survey --format=compact`, which provides:
1. **Signatures** with **annotations** — pre-grouped error patterns with deterministic structural observations
2. **Envelope patterns** — deduplicated infrastructure signal (one per failure mode, not per run)
3. **Fleet metrics** — pass rates, deploy correlation, regional/temporal patterns

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
go build -o /tmp/arohcp-ci-triage ./tooling/arohcp-ci-triage/
```

All output is JSON. GCS artifacts are cached locally — repeat scans are sub-second.

## Workflow

### Phase 1: Gather

Run survey commands with `--format=compact`. All commands in parallel.

**periodic scope:**
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=all --days=7
/tmp/arohcp-ci-triage survey --format=compact --env=int --job=nightly --days=7
/tmp/arohcp-ci-triage survey --format=compact --env=prod --job=nightly --days=7
```

**presubmit scope:**
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=dev --days=5
```

**all scope:** Both periodic + presubmit in parallel.

### Phase 2: Assess

Read the compact data in this order:

**1. Fleet health** — `status` (pass_rate, streak, baseline, failure_stage) and `daily_rates` (trends). Compare `pass_rate` to `baseline.pass_rate` — if baseline verdict is "DEGRADED", say so definitively. If "normal", the current rate is within historical range. Use `failure_stage` to lead with whether failures are provision (infra) or E2E (tests).

**2. Data reliability** — `data_window.truncated` (incomplete data?), `ev2_coverage` (deploy correlation reliable?).

**3. Signatures with annotations** — each signature is one failure pattern. Read:
- `key` — normalized error (for grouping only, values stripped)
- `representative_error` — full unmodified error text (the real signal)
- `hit_count`, `test_count`, `test_hits` (map of test name → per-test failure count) — scope and distribution of impact
- `annotations` — pre-computed structural observations (see below)
- `first_failure`, `last_failure` — temporal bounds

**4. Annotations** — factual labels on signatures, NOT severity judgments. You decide significance:

| Annotation | Meaning | Your job |
|---|---|---|
| `cascade` | Median >15 failures/run. Single root cause amplified. | Treat as 1 problem, not N test failures |
| `self_resolved` | Last hit in first half of window AND ≥5 hits | Verify: is it really gone, or paused? |
| `cross_env:N` | Same error in N environments | N=3 likely code/infra, N=2 investigate |
| `provision_spread:N_prs` | Provision failures across N unique PRs | Infrastructure issue blocking developers |
| `chronic_all_hashes` | All EV2 hashes <50% pass rate | NOT deploy-correlated, chronic issue |
| `deploy_onset` | Pass→fail at a hash transition | Deploy regression candidate |

**Annotations are observations, not conclusions.** Override when domain knowledge contradicts — e.g., `chronic_all_hashes` on a nightly job may be expected because OCP candidate channels are inherently less stable.

**5. Envelope patterns** — deduplicated infrastructure signal. Each pattern represents N runs with the same failure mode:
- `run_count` — how many runs share this pattern
- `exit_code`, `oom` — pod-level outcome
- `error_chain` — full ci-operator failure chain
- `lease_wait_s_range`, `pod_sched_s_range` — [min, max] infrastructure latency
- `failed_steps` — which CI pipeline steps failed
- `provision_fail_types` — top-3 normalized provision failure types with counts
- `build_log_error_sample` — first 2 ERROR/FATAL lines

**6. Deploy correlation** — `ev2_hash_rates` (per-deploy pass rates), `ev2_onsets` (pass→fail transitions).

**7. Cross-env** — `cross_env_signatures` (same error across environments), `cross_env_failures` (same test across environments).

### Phase 3: Diagnose and report

For each signature, apply diagnostic reasoning:

- **Merge related signatures** — same pattern in 3 envs = 1 finding, not 3
- **Assess legitimacy** — use `baseline.pass_rate` to determine if the current state is a regression. If `baseline.verdict` is "DEGRADED", this IS a regression, not normal variance. Don't say "INCONCLUSIVE" when the baseline gives a definitive answer.
- **Distinguish mechanisms** — provision_fail_types shows whether failures are ARM throttling, MSI connectivity, EventGrid, etc. Don't collapse distinct mechanisms
- **Identify low-volume qualitative signals** — a 4-hit PROD-only finding can be more important than a 100-hit presubmit cascade
- **Express uncertainty** — "INCONCLUSIVE: need baseline" is a valid assessment. Don't force CRITICAL on everything with high hit counts

**For presubmit PR analysis:** Check `runs[].pr` field. Failures spread across many PRs = infrastructure. Concentrated on 1-2 PRs = PR-specific.

**Every failure must appear in the report.** Cluster by signature.

## Signal Absence as Evidence

- No `cross_env_signatures` → environment-specific
- `chronic_all_hashes` annotation → not deploy-correlated
- All envelope patterns show `lease_wait_s_range` < [0, 2] → no Boskos contention
- All envelope patterns show `oom: false` → no memory issues
- `region_rates` uniform → not region-specific
- `provision_fail_types` empty → provisioning succeeded, failure in E2E tests

## Cross-Run-Type Correlation

- **Periodic timeout + presubmit provision failure** → shared infra
- **Same signature in periodic + presubmit** → code bug, not environment
- **Periodic fails, presubmit passes** → environment or deploy issue
- **Presubmit fails, periodic passes** → PR code issue or ephemeral infra
- **Nightly fails with different tests** → OCP candidate regression, not ARO

## Command Budget

**periodic:** 3 max. **presubmit:** 1. **all:** 4 max.

## Data Reference (compact format)

### Per-environment data

- **status**: `{streak, current_green, pass_rate, total_runs, baseline, failure_stage}`
  - `baseline`: `{pass_rate, total_runs, days, verdict}` — rolling pass rate from runs BEFORE the current window. `verdict` is "DEGRADED" (>15pp drop), "improved" (>15pp gain), or "normal". Use this to distinguish real regressions from normal variance.
- **data_window**: `{requested_days, actual_days, truncated}`
- **daily_rates**: `[{date, pass, total}]`
- **ev2_coverage**: `{with_ev2, total}`
- **ev2_hash_rates**: `[{hash, pass, fail, total, pass_rate, is_cron}]`
- **failure_scale_dist**: `{none, isolated, moderate, cascade}`
- **region_rates**: `[{region, pass, total, pass_rate, low_sample}]`
- **runs**: Minimal metadata only. `[{id, ts, fail, hash, pr, url}]`
- **status.failure_stage**: `{provision, e2e, build, other}` — counts of failed runs by stage. Provision=ARM deployment failed before tests. E2E=tests ran but failed. Build=image build failed. Use for the quick "what kind of failure?" answer.
- **signatures**: `[{key, hit_count, test_count, test_hits:{test_name: count}, first_failure, last_failure, best_run_id, best_run_url, representative_error, annotations[]}]`
  - `test_hits` is a map of test name → per-test failure count. Use for "affected tests" tables sorted by count.
- **envelope_patterns**: `[{run_count, exit_code, oom, error_chain, lease_wait_s_range, pod_sched_s_range, failed_steps[], provision_fail_count, provision_fail_types[{pattern, count}], arm_error_codes[{code, count}], alert_count, build_log_error_sample[], example_run_ids[], example_run_urls[]}]`
  - `arm_error_codes` names specific Azure ARM errors: RoleAssignmentLimitExceeded (quota), ValidateVMScaleSetOperation (throttling), DeploymentFailed (generic), etc.
- **ev2_onsets**: `[{test_name, last_pass_hash, first_fail_hash, last_pass_run, first_fail_run, last_pass_time, first_fail_time}]`
- **cross_env_failures**: `[{test_name, env_count, environments[{env, hits, run_id}]}]`
- **cross_env_signatures**: `[{key, env_count, total_hits, environments[{env, hits, run_id}]}]`

### What's NOT in compact format

- **No `failures[]` array** — signatures cover 100% of tests via `test_hits` map. Use `representative_error` for error detail.
- **No per-run envelopes** — replaced by `envelope_patterns` (deduplicated, one per failure mode). Cite patterns, not individual runs.
- **No per-run region/cluster/SHA** — available in full format if needed.

## Domain Knowledge

**Env mapping:** int=aro-integration, stg=aro-stage, prod=aro-production

**Three CI run types:**

| Type | Job pattern | Trigger | Query |
|---|---|---|---|
| Presubmit (ephemeral) | `pull-ci-*-e2e-parallel` | PR push | `--env=dev` |
| Presubmit (shared env) | `pull-ci-*-{stage,integration,prod}-e2e-parallel` | `/test` command | `--env=dev --job=stage` |
| Periodic | `periodic-ci-*-e2e-parallel` | EV2 deploy / cron | `--env=int/stg/prod` |

**EV2 annotations:** `ev2.rollout/ARO-HCP` = deploy hash, `ev2.rollout/region` = target region. Periodic only.

**Nightly runs:** Separate section. High expected failure rate (candidate OCP). Query with `--job=nightly`. Note nightly-specific failures. OCP candidate channels are inherently less stable — don't flag nightly failures as Critical without evidence of regression.

## Output

Write the full report as markdown. Structure:

```markdown
# CI Fleet Assessment — [scope] · [window] · [date]

## Fleet Status

| Env | Pass Rate | Runs | Trend | Severity |
|---|---|---|---|---|
| INT | 72.7% | 11 | `▇▇▅▇▇▇▅▇` | OK |

Sparkline: one bar per day from daily_rates. ▁<20% ▃20-50% ▅50-80% ▇>80%
Severity: <30% = Critical, 30-60% = Warning, >60% = OK

<details>
<summary>All failed runs (N)</summary>

| Env | Run | Time | Hash | Failures | Prow |
|---|---|---|---|---|---|
| PROD | 2048175228365836288 | 04/24 18:17 | a16d7bc6c464 | 29 | [link](url) |

</details>

## Findings

### P1 — [Title] `[ENV]` `[ENV]`

**Error:** `normalized error signature`

| Dimension | Value |
|---|---|
| Trend | sparkline + description |
| Tests affected | N tests |
| Cross-env | envs and hit counts |
| Deploy correlated | Yes/No — cite ev2_onsets or ev2_hash_rates |

**Evidence chain:**
1. [signatures:key] "..." — Nx across N tests
2. [signatures:annotations] cascade, cross_env:3
3. [envelope_patterns] N runs: exit_code=1, oom=false, error_chain="..."
4. [envelope_patterns:lease_wait_s_range] [0.9, 1.6] — no contention
5. [envelope_patterns:provision_fail_types] cluster: 59x, geneva: 27x
6. [ev2_hash_rates] hash1=25%, hash2=0% — all hashes fail
7. [cross_env_signatures] same key in STG (145x) + PROD (152x)

**Hypothesis:** [interpretation — connect evidence to root cause]

**Action:** [what to do next]

<details>
<summary>Affected tests (N)</summary>

| Count | Env | Test | Error |
|---|---|---|---|
| 11x | PROD | test name | error summary |

</details>

<details>
<summary>Deploy timeline</summary>

| Time | Hash | Result | Detail |
|---|---|---|---|
| 04/23 00:08 | `3e5c4d579c24` | FAIL | 9 failures |
| 04/24 18:17 | `a16d7bc6c464` | FAIL | 29 — **new deploy** |

</details>

<details>
<summary>Top runs</summary>

| Run | Env | Failures | Prow |
|---|---|---|---|
| 2048175228365836288 | PROD | 34 | [link](url) |

</details>

## Nightly

[Separate nightly analysis if applicable]

## Coverage Gaps

[Note any data reliability issues]
```

**Rules:**
- Evidence chain = numbered facts citing source fields. Hypothesis = interpretation. Keep them separate.
- Cite `envelope_patterns` (aggregated) not individual run envelopes.
- Cite `annotations` in evidence chains — they're pre-computed structural facts.
- All failed runs table: every failed run from `runs[]`.
- Affected tests: every test from `signatures[].test_hits`, sorted by count descending.
- Deploy timeline: runs sorted by timestamp, bold hash transitions.

## Error Handling

| Situation | Action |
|---|---|
| Sippy down | Note "unavailable." Report what you have. |
| `data_window.truncated` | State actual window. Don't present as full baseline. |
| No failures | Fleet healthy. Report. |
| EV2 coverage < 50% | Note — deploy correlation unreliable. |
| `envelope_patterns` empty | Note — GCS artifacts unavailable. |
