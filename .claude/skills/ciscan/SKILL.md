---
name: ciscan
description: "CI fleet assessment — wide-first signal discovery and correlation"
---

# CI Fleet Assessment

Discover, correlate, and prioritize CI failure signals across environments. You are the wide net — you find what's worth investigating and produce a fleet assessment.

You do NOT inspect per-run artifacts. That's `/cidig`. Your tool is `survey`.

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
2. Have a signal group from a prior `/ciscan` report → `/cidig`
3. Have a specific test name, error message, or PR number → `/cidig`
4. Everything else ("how is CI?", "fleet status", "what's broken?") → `/ciscan` (you are here)

## Setup

```bash
go build -o /tmp/citriage ./tooling/citriage/
```

All output is JSON.

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

### Step 3: Count distinct problems (not tests)

Read the `outputs[].error` across all `failures` and cluster by reading the full error text. Tests with the same error mechanism are one problem.

**Output capping:** The top 20 failures by frequency have full error text in `outputs[]`. Lower-ranked failures have name + count only. If you need error text for a lower-ranked failure, note it for cidig drill-down using its `best_run_id`.

**Clustering rules:**
- Read the **full** error text, not just the prefix. "failed to create HCP cluster" can mean timeout, TLS mismatch, or quota — the difference is deep in the message.
- **Pipeline step errors** (`Run pipeline step ...`): check `outputs[].extracted_errors` first — these are the ERROR/FATAL lines only. The actual diagnostic is inside the `err="..."` field, often an ARM JSON body. Quote the specific error code, not the wrapper.
- When a test appears in `cross_env_failures`, compare error text **per environment** before assuming shared root cause. Same test in INT and PROD can fail for different reasons.
- Different source files but same operational failure = one problem.
- Similar text but different operations = distinct problems.
- When unsure, keep them separate.

**Multi-mechanism de-duplication:** When the same test fails with different error mechanisms across runs (e.g., timeout on run A, TLS mismatch on run B), count the test ONCE under its primary (most frequent) mechanism. Note the secondary mechanism as a variant within that finding, not a separate finding. Never let the same test appear in two findings with its full failure_count in both — that inflates the apparent scope.

**Co-failure detection:** Tests with identical `failure_count` and matching `first_failure`/`last_failure` windows are likely co-failing. Also check: do the same `outputs[].run_id` values appear across multiple failures? Tests that consistently share failing runs have a shared mechanism.

**Trend direction:** Check `daily_rates` — is it getting worse, improving, or flat within the window? `last_pass` after `last_failure` = intermittent. Otherwise = persistent. Don't speculate about what happened before the window — this tool answers "what's broken now," not "when did it start."

### Step 4: Correlate each finding

For each distinct problem:

- **Cross-env** — in `cross_env_failures`? Multi-env = code/infra. Single-env = environment-specific.
- **Deploy correlation** — check `ev2_hash` values in the `runs` array. If the hash changed between the last passing run and first failing run (by timestamp), note the deploy. If failures appear across many hashes, it's not deploy-correlated. Build the deploy timeline from `runs[]` in timestamp order — each row is one run with its timestamp, hash, result, and failure count. Show hash transitions so the reader can see deploy boundaries.
- **Region** — check `region_rates`. Dramatically lower pass rate in one region = region-scoped.
- **Duration** — ~2700s = timeout ceiling. ~600s = fast failure. Consistent = deterministic.
- **Temporal** — multiple distinct problems sharing onset within 8 hours = shared trigger.
- **Upstream** — only for OCP/platform bugs (TLS, DNS, API server errors). Check `https://sippy.dptools.openshift.org/api/tests?search=ERROR_TEXT&release=aro-integration`.

### Step 5: Assess presubmit (presubmit and all scopes only)

Skip for periodic scope.

- **PR concentration** — check `pull_number` on failing runs. Failures from 1-2 PRs = those PRs are broken, fleet is healthy. Failures spread across many PRs = infrastructure issue.
- **Pipeline step failures** (`Run pipeline step ...`) block all PR testing — highest urgency. Read `extracted_errors` for the actual ARM error code.
- **Cross-reference** test failures against periodic findings. Same test + same error = shared root cause.

**PR deduplication:** A heavily-retested PR inflates counts. Report **distinct PRs affected** alongside raw counts: "31 hits across 15 PRs" not just "31 hits." Flag outlier PRs (>5 runs).

### Step 6: Prioritize, report, and dig

Order findings by impact. Consider: test count, environment count, presubmit impact, regression vs chronic.

**Every failure must appear in the report.** Cluster by mechanism, but list ALL tests per finding (including 1x singletons). Prefix each with `[PROD]`, `[STG]`, `[INT]`, or `[PRESUBMIT]`.

For the **top 2-3 findings**, launch `/cidig` investigations in parallel as background agents. Use the `best_run_id` from each finding. Skip if the user says "just scan."

## Failure Mode Decision Trees

When classifying findings:

**Timeout failures** (duration ~2700s, "timeout", "context deadline exceeded"):
- Check `daily_rates` for temporal onset
- Check `ev2_hash_rates` for deploy correlation
- Check `region_rates` for region skew
- Cross-env? → platform issue. Single-env? → environment-specific.
- All hashes failing? → not deploy-correlated (cron runs too = chronic)

**ARM/Azure API failures** ("ERROR CODE", "ResponseError", ARM JSON body):
- Read the full ARM error body — the error code IS the diagnosis
- Common: RoleAssignmentLimitExceeded, QuotaExceeded, ResourceNotFound
- Single-env = config/quota. Multi-env = API/service issue.

**Pipeline step failures** ("Run pipeline step ..."):
- Always read `extracted_errors` first
- These are infra provisioning, not test code
- Highest urgency for presubmit — blocks all PR testing

**Cascade patterns** (`failure_scale_dist` shows many cascade runs):
- 30 tests with one error = 1 problem. Count errors, not tests.
- Typically: cluster creation timeout cascading to all dependent tests

## Signal Absence as Evidence

What you DON'T see is diagnostic:
- No `cross_env_failures` for a test → environment-specific issue
- `ev2_hash_rates` ALL hashes failing → not deploy-correlated (chronic or cron-only)
- `region_rates` uniform → not region-specific
- `failure_scale_dist` mostly "none" with few "cascade" → isolated regressions, not systemic
- High `ev2_coverage` but no hash correlation → code change didn't cause it
- `last_pass` well after `first_failure` → intermittent, not permanent regression

## Cross-Run-Type Correlation

Patterns that span run types reveal root cause layer:
- **Periodic timeout + presubmit provision failure** → shared infra (EventGrid, Maestro, regional pipeline)
- **Same test fails periodic + presubmit** → code bug, not environment
- **Periodic fails, presubmit passes** → environment or deploy issue
- **Presubmit fails, periodic passes** → PR code issue or ephemeral infra
- **Nightly fails with different tests** → OCP candidate regression, not ARO
- **Nightly + periodic same errors** → ARO issue manifesting on both OCP versions

## Command Budget

**periodic:** survey --env=all (1) + nightly INT (1) + nightly PROD (1) = **3 max**
**presubmit:** survey --env=dev (1) + shared-env (1 if requested) = **2 max**
**all:** Both = **5 max**

## Data Reference

### `survey --env=all --days=N`

Per-environment data:
- **status**: `{streak, current_green, streak_regions, pass_rate, total_runs}`
- **data_window**: `{requested_days, actual_days, oldest_run, newest_run, truncated}`
- **daily_rates**: `[{date, pass, total}]`
- **ev2_coverage**: `{with_ev2, total}`
- **ev2_hash_rates**: `[{hash, pass, fail, total, pass_rate, is_cron}]` — sorted by total desc. `is_cron: true` for NO_HASH. Use to identify bad deploys (hash with 0% pass rate).
- **failure_scale_dist**: `{none, isolated, moderate, cascade}` — run count by failure bucket: none=0, isolated=1-3, moderate=4-15, cascade=16+.
- **region_rates**: `[{region, pass, total, pass_rate, low_sample}]` — `low_sample: true` when total < 3.
- **runs**: FAILED RUNS ONLY. `[{id, timestamp, overall_result, real_failures, ev2_hash, region, cluster, pull_number, sha, url}]` — passing runs are captured in the aggregate arrays above. `sha` is the PR commit SHA (presubmit only) — use to distinguish "same PR, different push" from "same PR, retested same code."
- **failures**: `[{test_name, failure_count, first_failure, last_failure, last_pass, best_run_id, best_run_url, total_runs, outputs[{run_id, error, extracted_errors}]}]` — sorted by frequency desc. **Top 20 have full `outputs[]`. Remaining have name + count only (no outputs).** `extracted_errors` contains ERROR/FATAL lines for pipeline step tests.
- **cross_env_failures** (in `--env=all`): `[{test_name, env_count, environments[{env, hits, run_id}]}]` — error text is in per-env `failures`, look up by test name.

## Domain Knowledge

**Env mapping:** int=aro-integration, stg=aro-stage, prod=aro-production

**Three CI run types:**

| Type | Job pattern | Trigger | Query |
|---|---|---|---|
| Presubmit (ephemeral) | `pull-ci-*-e2e-parallel` | PR push | `--env=dev` |
| Presubmit (shared env) | `pull-ci-*-{stage,integration,prod}-e2e-parallel` | `/test` command | `--env=dev --job=stage` |
| Periodic | `periodic-ci-*-e2e-parallel` | EV2 deploy / cron | `--env=int/stg/prod` |

**EV2 annotations:** `ev2.rollout/ARO-HCP` = deploy hash, `ev2.rollout/region` = target region. Periodic only. INT ~100%, STG ~42%, PROD ~88%.

**Nightly runs:** Separate section. High expected failure rate (candidate OCP). Query with `--job=nightly`. Note nightly-specific failures not seen in periodic.

**Do not maintain a mental list of known failures.** Read the error text. Cluster by similarity. Report what you see.

## Output

### Step 7: Publish HTML report

**Template:** `tooling/citriage/report-template.html`. Contains CSS, JS `render()`, and `const DATA = null;` placeholder. Do NOT read or copy the template — just write the DATA JSON and run the injection command.

**How:**
1. Write DATA object (JSON) to `/tmp/ciscan-data.json`
2. Inject into template: `sed "s|const DATA = null;|const DATA = $(python3 -c "import json,sys;print(json.dumps(json.load(sys.stdin)))" < /tmp/ciscan-data.json);|" tooling/citriage/report-template.html > /tmp/ciscan-report.html`
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

**Deploy timeline rows:** Each row is ONE RUN in timestamp order — `ts` is the actual run time (e.g., `"04/25 23:00"`), `hash` is its EV2 hash, `pass`/`fail` is the result, `meta` is a brief note (e.g., `"34 failures"`). Set `newDeploy: true` when the hash changes from the previous row. Do NOT aggregate into "hash totals" — the reader needs the chronological story to see deploy boundaries.

**Findings:** `evidence` is REQUIRED — cite source and value. `relatedFailures` is REQUIRED and EXHAUSTIVE — every test, prefixed with `[ENV]`, sorted by count.

**In conversation:** Brief summary (fleet status, finding count). HTML report is the deliverable.

## Error Handling

| Situation | Action |
|---|---|
| Sippy down | Note "unavailable." Report what you have. |
| `data_window.truncated` | State actual window. Don't present as full baseline. |
| No failures | Fleet healthy. Report. |
| EV2 coverage < 50% | Note — deploy correlation unreliable. |
