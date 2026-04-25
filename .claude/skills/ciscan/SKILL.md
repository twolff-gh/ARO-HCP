---
name: ciscan
description: "CI fleet assessment — wide-first signal discovery and correlation"
---

# CI Fleet Assessment

Discover, correlate, and prioritize CI failure signals across environments. You are the wide net — you find what's worth investigating and produce a fleet assessment.

You do NOT inspect per-run artifacts. That's `/cidig`. Your tools are `survey` and `search`.

## Arguments

```
/ciscan [SCOPE] [DAYS]
```

- **SCOPE** — `periodic` (default), `presubmit`, or `all`
  - `periodic` — INT, STG, PROD environments. Production health, deploy correlation. The most common ask.
  - `presubmit` — Ephemeral PR test environments. Developer velocity, pipeline/build blockers, PR concentration.
  - `all` — Both. Produces a longer report. Use when the user says "everything" or "how is CI?"
- **DAYS** — Lookback period (default: 7 for periodic, 5 for presubmit)

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

Follow these steps in order. Each step builds on the previous one.

### Step 1: Gather fleet data

Run the survey(s) for the requested scope. Adapt `--days` to the user's requested timeframe.

**periodic scope:**
```bash
/tmp/citriage survey --env=all --days=7
# Nightly (always include alongside periodic):
/tmp/citriage survey --env=int --job=nightly --days=7
/tmp/citriage survey --env=prod --job=nightly --days=7
```
Run the nightly surveys in parallel with the main periodic survey.

**presubmit scope:**
```bash
/tmp/citriage survey --env=dev --days=5
```

**all scope:** Run periodic, nightly, and presubmit surveys in parallel.

Only add `survey --env=dev --job=stage --days=7` if the user specifically asks about shared-env presubmit — these runs are rarely triggered and usually return empty data.

### Step 2: Check data reliability

Before analyzing, verify the data:

- **`data_window.truncated`** — if true, Sippy returned less data than requested. State the actual window ("requested 7 days, got 5") and do not present pass rates as representative of the full period.
- **`ev2_coverage`** — if `with_ev2 / total < 50%`, deploy correlation is unreliable for that env. Note it.

### Step 3: Count distinct problems (not tests)

Read the `error_groups` across all `failures` and cluster them yourself by error similarity. Tests whose dominant error text describes the same mechanism are one problem — report them as one finding, not N.

Guidelines for clustering:
- Read the **full** error text in `error_groups`, not just the prefix. Errors that start with the same phrase (e.g., "failed to create HCP cluster") can have completely different root causes deeper in the message (e.g., timeout vs TLS cert mismatch vs quota exhaustion). Always read to the end.
- When a test appears in `cross_env_failures`, compare the full `error_groups` text **per environment** before assuming they share a root cause. The same test name in INT and PROD can fail for entirely different reasons — only the error text reveals this.
- Errors at different source files but with the same operational failure (e.g., all timing out on cluster creation) are one problem.
- Errors that look similar but name different operations (e.g., cluster creation timeout vs credential access timeout) are distinct problems.
- When unsure, keep them separate — it's better to split than to merge incorrectly.

Then check `co_failure_groups`. Tests with >80% co-occurrence overlap have a shared mechanism. If your error-based clusters and co-failure groups agree, you have high confidence they're the same issue.

Only drill into individual `failures` entries for tests that don't cluster — these are distinct problems.

**Chronicity:** As you cluster, note each finding's chronicity:
- `at_window_boundary: true` → chronic (predates the survey window). State it and move on.
- `first_failure` well inside the window → regression. Note the onset date.
- `co_failure_groups[].onset` near window start → chronic. Well inside → regression.

Only run a 30-day baseline survey (`survey --env=ENV --days=30`) when a finding looks like a regression AND you need to confirm it didn't exist before. Budget: up to 3 single-env 30-day surveys. Always check `data_window.truncated` on 30-day results.

### Step 4: Correlate each finding

For each distinct problem, check these dimensions:

- **Cross-env** — in `cross_env_failures`? Multi-env = code/infra issue. Single-env = environment-specific.
- **Deploy correlation** — check `ev2_hash` values in the `runs` array near `first_failure`. If the hash changed between a passing and failing run, note the deploy. If the failure appears across many different hashes, it's not deploy-correlated. (Detailed deploy inspection belongs in `/cidig`, not here.)
- **Region** — check `region_rates`. Dramatically lower pass rate in one region = region-scoped.
- **Duration** — ~2700s = timeout ceiling. ~600s = fast failure. Consistent = deterministic. High variance = intermittent.
- **Temporal** — multiple distinct problems sharing onset within 8 hours = shared trigger.
- **Upstream bug check** — only when a finding looks like an OCP or platform bug (TLS/certificate errors, DNS issues, API server failures — not ARO-specific errors like cluster creation timeouts). Run `search "exact error message" --age=168h` and check the `issues` array for existing OCPBUGS tickets. Skip this for errors that are clearly ARO-specific (timeouts on CreateHCPCluster, pipeline step failures, test-specific assertions).

### Step 5: Assess presubmit platform (presubmit and all scopes only)

Skip this step for periodic scope.

From the dev survey:
- **PR concentration** — check `pull_number` on failing runs. If most failures come from 1-2 PRs, the fleet is healthy and those PRs are broken. If failures are spread across many PRs, it's an infrastructure or shared-code issue. Report the distinction — it changes the triage decision.
- **Pipeline step failures** (`Run pipeline step ...`) and **build failures** (`Build image ...`) block all PR testing — highest-urgency presubmit issues.
- **Test failures** — cross-reference against periodic findings. Same test + same full error text = shared root cause.

**PR deduplication (important):** A heavily-retested PR (e.g., 17 runs) inflates hit counts and distorts pass rates. When computing presubmit statistics:
- Count **distinct PRs affected** alongside raw run/hit counts. Report both: "31 hits across 15 PRs" not just "31 hits."
- **Flag outlier PRs** — any PR with >5 runs in the window. Note its contribution to the totals so readers can mentally adjust.
- For each presubmit finding's **Affected PRs** section, show: PR number (linked), finding-specific hit count, total run fail rate, and whether the failures are fleet-wide (same errors across many PRs) or PR-specific.
- **Never present raw run-count pass rates as fleet health** without noting PR distribution. "45.8% pass rate across 214 runs" is misleading if one PR contributed 17 of those runs.

### Step 6: Prioritize, report, and dig

Order findings by impact. Consider: test count affected, environment count, presubmit impact, regression vs chronic.

**Every failure must appear in the report.** Findings are clustered by error mechanism, but each finding's `relatedFailures` section must list ALL tests that fall under that cluster — including singletons (1x). Tests that don't cluster into any finding get their own P3 finding. The reader must be able to see the full inventory of failures, not just the top N.

For each failure entry in `relatedFailures`, prefix with the environment tag: `[PROD]`, `[STG]`, `[INT]`, or `[PRESUBMIT]`. Sort by hit count descending within each finding.

For the **top 2-3 findings**, launch `/cidig` investigations in parallel as background agents. Pick the best failing run ID for each finding (most failures, most recent) and include the known context from your scan. The cidig skill will use `triage <run-id>` as its entry point — a single call that extracts all artifact signals with cascade detection and error deduplication. If the user says "just scan" or "don't dig," skip this.

## Command Budget

**periodic scope:**

| Command | Max calls |
|---------|-----------|
| `survey --env=all --days=N` | 1 |
| `survey --env=int --job=nightly --days=N` | 1 |
| `survey --env=prod --job=nightly --days=N` | 1 |
| `survey --env=ENV --days=30` | 3 (one per env with failures) |
| `search "pattern"` | 2 (upstream bug check only) |
| **Total** | **8** |

**presubmit scope:**

| Command | Max calls |
|---------|-----------|
| `survey --env=dev --days=N` | 1 |
| `survey --env=dev --job=stage` | 1 (only if user requests shared-env presubmit) |
| `search "pattern"` | 2 (upstream bug check only) |
| **Total** | **4** |

**all scope:** Both budgets apply (total 12).

## Data Reference

### `survey  --env=all --days=N`

Per-environment data:
- **data_window**: `{requested_days, actual_days, oldest_run, newest_run, truncated}`
- **status**: `{streak, current_green, streak_regions, pass_rate, total_runs}`
- **daily_rates**: `[{date, pass, total}]`
- **ev2_coverage**: `{with_ev2, total}`
- **ev2_hash_rates**: `[{hash, pass, fail, total, pass_rate, is_cron}]` — per-EV2-hash pass/fail rates. `is_cron: true` for NO_HASH (cron-triggered) runs. Use to identify bad deploys (hash with 0% pass rate) and chronic cron failures. Sorted by total runs descending.
- **failure_scale_dist**: `{none, isolated, moderate, cascade}` — count of runs by failure scale bucket: none=0 failures, isolated=1-3, moderate=4-15, cascade=16+. Tells you the shape of failures — mostly isolated regressions vs systemic cascades.
- **region_rates**: `[{region, pass, total, pass_rate, low_sample}]` — only for envs with EV2 region annotations. `low_sample: true` when total < 3 runs — treat pass rates as anecdotal, not statistically meaningful.
- **runs**: `[{id, timestamp, overall_result, real_failures, failed_tests[], ev2_hash, region, pull_number, url}]`
  - `pull_number`: PR number extracted from the Prow URL (presubmit only; 0 for batch merges and periodic runs). Use to distinguish "one bad PR" from fleet-wide issues.
- **failures**: `[{test_name, failure_count, regular_hits, first/last_failure, last_pass, regions, at_window_boundary, error_groups[{error, count}], normalized_error, daily_hits[{date, count}], durations[] (top 10 only, last 7 values in seconds), best_run_id, best_run_url, total_runs}]`
  - `at_window_boundary`: true when `first_failure` is within 24h of the survey window start — the failure likely predates the window (chronic)
  - `chronicity`: `"at_boundary"` (predates window, chronic), `"within_window"` (appeared during window, regression candidate), or `"intermittent"` (last_pass is after last_failure, flaky). More precise than `at_window_boundary` alone.
  - `normalized_error`: the dominant error text with instance-specific noise stripped (source locations, timestamps, UUIDs, Azure URLs, numeric values, hex addresses, lowercased). Use this to cluster tests by error similarity — tests with similar `normalized_error` values share the same failure mechanism.
- **co_failure_groups**: `[{leader, members[{test, total_failures, co_failures, solo_failures}], min_overlap_pct, distinct_runs, common_run_ids[], common_ev2_hashes[], common_regions[], onset}]`
  - `distinct_runs`: total number of runs where 2+ members co-failed (before `common_run_ids` is capped at 10). Low count with many members = concentrated in a few bad runs.
- **co_failure_stats**: `{threshold, adapted_threshold, blast_radius, blast_excluded, eligible_tests, pairs_checked, pairs_above_threshold, max_overlap_pct, reason}`
  - `adapted_threshold`: when eligible tests have ≤5 runs each, threshold is automatically lowered to 0.5 to compensate for sparse data. This field is only present when adaptation occurred.
- **cross_env_failures** (top-level in `--env=all`): `[{test_name, env_count, environments[{env, hits, run_id}]}]` — error details are in the per-env `failures` array, look up by test name

- **pr_stats** (presubmit only, omitted for periodic): `{distinct_prs, pr_weighted_pass_rate, run_weighted_note, prs[]}`
  - `pr_weighted_pass_rate`: average of each PR's individual pass rate — not skewed by heavily-retested PRs. Compare against `status.pass_rate` (run-weighted) to detect skew.
  - `run_weighted_note`: present when an outlier PR (>5 runs) significantly skews the run-weighted pass rate. Explains the discrepancy.
  - `prs[]`: `{number, runs, passed, failed, pass_rate, failed_tests[], outlier, url}` — sorted by failure count descending. `failed_tests` lists distinct test names that failed for this PR (use to map PRs to findings). `outlier: true` when runs > 5.
  - Use `pr_stats` for the **Affected PRs** section of each presubmit finding: cross-reference each finding's test names against `prs[].failed_tests` to build the per-finding PR table.

### `search  "pattern" --age=168h --env=ENV`

Returns: `{pattern, search_url, total_tests, aro_count, other_count, truncated, groups[{file, names[], context[], urls[], count}], issues[{name, url}]}`

## Domain Knowledge

**Env mapping:** int=aro-integration, stg=aro-stage, prod=aro-production

**Three CI run types:**

| Type | Job pattern | Infra | Trigger | Query |
|---|---|---|---|---|
| Presubmit (ephemeral) | `pull-ci-*-e2e-parallel` | Provisions own env | PR push | `--env=dev` |
| Presubmit (shared env) | `pull-ci-*-{stage,integration,prod}-e2e-parallel` | Shared INT/STG/PROD | `/test e2e-stage-parallel` | `--env=dev --job=stage` |
| Periodic | `periodic-ci-*-{stage,integration,prod}-e2e-parallel` | Shared INT/STG/PROD | EV2 deploy / cron | `--env=int` / `--env=stg` / `--env=prod` |

**EV2 annotations:** `ev2.rollout/ARO-HCP` = deploy hash, `ev2.rollout/region` = target region. Only on periodic runs. INT has ~100% coverage, STG ~42%, PROD ~88%.

**Nightly runs:** Always included as a separate section. High expected failure rate (candidate OCP). Query with `--job=nightly`. Summarize pass rate and call out nightly-specific failures (tests not seen in periodic), but defer full clustering until fleet-wide issues resolve.

**Do not maintain a mental list of known failure modes.** Read the `error_groups` on each failure — they show what's actually failing right now with full error text. Cluster by error similarity yourself. Report what you see, not what you expect.

## Output Format

The primary output is the HTML report (Step 7). In-conversation text should be a brief summary pointing the user to the report.

**In conversation:** After generating the report, output a short summary: fleet status one-liner per env, number of findings by priority, and note that the HTML report is open. Don't reproduce the full report as text — the HTML is the deliverable.

Two valid outcomes:
1. **Findings that need attention** — HTML report with findings, plus cidig investigations launched for the top findings
2. **Fleet healthy** — all environments passing, note streak lengths. Still produce the HTML report (it's useful as a record).

### Step 7: Publish HTML report

After all analysis and cidig investigations complete, produce the HTML report.

**Template:** `report-template.html` in this skill's directory is a data-driven template. It contains all CSS, a JS `render()` function, and a `const DATA = null;` placeholder. You inject data by replacing that line.

**How to produce the report:**

1. Read `report-template.html`
2. Build the DATA object (JSON) following the contract in the template's HTML comment
3. Write a copy to `/tmp/ciscan-report.html`, replacing `const DATA = null;` with `const DATA = { ... };` containing your analysis
4. Open with `xdg-open /tmp/ciscan-report.html`

The DATA object shape (see template HTML comment for full contract):

```json
{
  "title": "CI Fleet Assessment",
  "subtitle": "All scopes · 7-day window · N findings",
  "generated": "2026-04-25T14:00:00Z",
  "fleetCards": [
    { "env": "int", "name": "Integration", "passRate": 53.0,
      "detail": "17/32 runs · streak: 1 red", "severity": "warning",
      "sparkline": [{"date": "04/19", "pct": 60, "color": "yellow"}] }
  ],
  "findings": [
    { "priority": "P1", "envs": ["int","stg","prod"],
      "title": "...", "error": "...",
      "dims": {"Chronicity": "Chronic", "Tests affected": "~15/env"},
      "hypothesis": "...", "action": "...",
      "deployTimeline": [{"env": "PROD", "rows": [
        {"pass": false, "ts": "04/22 09:10", "hash": "3e5c4d5", "meta": "3 failures", "newDeploy": false}
      ]}],
      "affectedPRs": [{"number": 4901, "url": "...", "hits": 5, "failRate": 80, "annotation": "fleet-wide"}],
      "relatedFailures": [{"count": 16, "name": "[PROD] no-CNI private cluster...", "error": "failed to create...", "url": "..."}],
      "evidence": ["Step 1: ...", "Step 2: ..."] }
  ],
  "nightly": [{"env": "Nightly INT", "passRate": 0, "runs": 6, "detail": "all failing"}],
  "coverageGaps": ["STG nightly not queried"]
}
```

**Severity thresholds:** passRate <30% = "critical" (red), 30-60% = "warning" (yellow), >60% = "ok" (green). Sparkline colors follow the same rule per day.

**Finding fields:**
- `priority`: "P1" (regression/degrading), "P2" (chronic/significant), "P3" (intermittent/resolving)
- `envs`: array of environment tags for filtering
- `dims`: key-value pairs for the dimensions grid (Chronicity, Tests affected, Deploy correlated, Region, etc.)
- `hypothesis`: use "Hypothesis" not "Root cause" — state what evidence would confirm it
- `deployTimeline`, `affectedPRs`: optional expandable sections
- `evidence`: **REQUIRED** — each step must cite the data source and specific values. Format: `"[source:field] observation — value"`. Examples: `"survey.env=prod.ev2_hash_rates: all hashes 0% — 3e5c4d5 0/7, 5276b68 0/2"`, `"survey.env=prod.runs[12]: cascade onset — 29 failures at hash a16d7bc, 04/24 18:17"`, `"error_groups[0].error: source file — cluster_create_complex_cilium_kv.go:104"`. When a finding came from a specific survey field, name the field. When an error points to a source file and line, include it.
- `relatedFailures`: **REQUIRED and EXHAUSTIVE** — every test that falls under this finding, prefixed with `[ENV]`, sorted by count descending. Include singletons. This is not a curated sample — it's the full inventory.

All sections render automatically from the DATA object. The template handles filtering, sparklines, expandable details, and styling.

## Error Handling

| Situation | Action |
|---|---|
| Sippy API down | Note "unavailable." Report what you have. |
| Search timeout or `truncated` | Retry with shorter `--age` or more specific pattern. Note if still fails. |
| `data_window.truncated: true` | State the actual window. Do not present as a full baseline. |
| No failures in survey | Fleet healthy. Report and stop. |
| EV2 coverage < 50% | Note in report — deploy correlation unreliable for that env. |
