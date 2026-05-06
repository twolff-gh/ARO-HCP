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

### Phase 1a: Screen

Run one summary command to get the fleet dashboard (~600 tokens per env):

**periodic/all scope:**
```bash
/tmp/arohcp-ci-triage survey --format=summary --env=all --days=7
```

**presubmit scope** (skip screening — single env, always fetch detail):
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=dev --days=5
```

### Phase 1b: Classify

Read the summary. Classify each environment using these rules **in order** — first match wins:

1. **NO_DATA**: `data_window.empty` → note and skip
2. **DEGRADED**: baseline verdict is `"DEGRADED"`, OR `pass_rate < 60%` → fetch compact + nightly
3. **INTERESTING**: `pass_rate < 80%`, OR any top_signature has a `cross_env` annotation → fetch compact (no nightly)
4. **GREEN**: none of the above → skip detail, one-line summary in report

If `cross_env_signature_count > 0`, **upgrade every env that contributes to a cross-env signature** to at least INTERESTING (fetch compact even if otherwise GREEN).

### Phase 1c: Fetch detail

Run all commands in parallel:

For each DEGRADED env:
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=<env> --days=7
/tmp/arohcp-ci-triage survey --format=compact --env=<env> --job=nightly --days=7
```

For each INTERESTING env:
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=<env> --days=7
```

**all scope:** Also run presubmit in parallel with the above:
```bash
/tmp/arohcp-ci-triage survey --format=compact --env=dev --days=5
```

For GREEN envs, include a one-line summary in the report ("INT: 88.6% pass rate, improved from baseline, 3 minor signatures — healthy") without fetching detail.

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

### Phase 4: Auto-drill (optional)

If Phase 3 produced findings with clear recommended runs AND the user hasn't asked for a quick/summary-only scan, drill into each finding's recommended run using sub-agents.

**For each finding** that has a recommended run (max 3 findings):

Spawn an Agent() call with this prompt template — all agents run **in parallel** (single message, multiple Agent() tool calls):

```
You are investigating a specific CI run to confirm or deny a hypothesis from fleet analysis.

## Finding context
- Title: {finding title}
- Signature: {signature_key}
- Hypothesis: {hypothesis}
- What to look for: {what_to_look_for list}
- Fleet context: {relevant fleet_context — cross-env, all_hashes_failing, nightly correlation}

## Instructions
1. Build the tool if needed: go build -o /tmp/arohcp-ci-triage ./tooling/arohcp-ci-triage/
2. Run: /tmp/arohcp-ci-triage triage {run_id}
3. Read the triage output and focus on: {what_to_look_for}
4. Follow this decision tree:

{Insert ONLY the relevant failure mode decision tree from cidig — e.g., Mode A for timeouts, Mode B for Azure errors. ~20 lines, not the full 298-line cidig SKILL.md.}

## Report format (keep it short — under 200 words)
CONFIRMED: {root cause} — {evidence from triage fields}
or DENIED: {why hypothesis is wrong} — {actual mechanism found}
or INCONCLUSIVE: {what's missing} — {suggested next step}

Include: lro_classification, key azure errors, neighbor pass rates, relevant step timings.
```

**Decision tree snippets to include per failure mode:**

For **timeout** findings (signature contains "timeout", "context deadline exceeded", "exceeded during"):
```
1. podinfo.oom_detected? → YES: OOM kill
2. metrics.max_lease_acq > 300? → YES: Boskos exhaustion
3. lro_classification?
   - "accepted_stuck" → CS layer blocked (deny assignment, CS crash)
   - "provisioning_stuck" → HyperShift/Maestro layer
   - "" → not enough azure.log data
4. neighbors.same_hash_passed > 0? → flake. == 0 → consistent failure
```

For **Azure API error** findings (signature contains "ResponseError", "ResponseFailed", ARM error codes):
```
1. azure[].response_errors — read specific error codes
2. provision (presubmit): same error? → infra setup failure
3. Common: ResourceNotFound (stale ref), QuotaExceeded (capacity), AuthorizationFailed (RBAC)
```

For **provision/pipeline step** findings (signature contains "pipeline step", "provision"):
```
1. steps[].name + failed=true — which step?
2. build_log.step_lines — step result message
3. provision.failures — full ARM error chain
```

For **other** failure modes: include the generic cidig triage summary — total/failed tests, error_groups, podinfo, events.

**Skip auto-drill when:**
- All findings are `self_resolved` (no active problem to confirm)
- The user asked for a quick scan or summary only
- No findings have recommended runs with sufficient signal

### Phase 5: Synthesize

After sub-agents return, integrate their results into the report:

1. **Update each finding** with the sub-agent verdict:
   - CONFIRMED → report as confirmed root cause with evidence
   - DENIED → report what the sub-agent found instead
   - INCONCLUSIVE → report what further investigation is needed

2. **Cross-finding correlation** — this is the unique value of having all results together:
   - Do multiple findings share a root cause? ("F1 and F2 both show accepted_stuck → single CS issue")
   - Does one finding explain another? ("F1's ARM throttling is a downstream effect of F2's CS blockage")
   - Are any findings independent? ("F3 is a test flake unrelated to F1/F2")

3. **Final report** — merge the Phase 3 report structure with Phase 5 verdicts. Each finding now has:
   - Fleet-level evidence (from Phase 3: signatures, annotations, deploy correlation)
   - Run-level evidence (from Phase 4: triage fields, lro_classification, azure errors)
   - Confirmed/denied/inconclusive status

## Signal Absence as Evidence

- No `cross_env_signatures` → environment-specific
- `chronic_all_hashes` annotation → not deploy-correlated
- All envelope patterns show `lease_wait_s_range` < [0, 2] → no Boskos contention
- All envelope patterns show `oom: false` → no memory issues
- `region_rates` uniform → not region-specific
- `provision_fail_types` empty → provisioning succeeded, failure in E2E tests
- `cascade_survivors` non-empty during cascade → coverage shadow: some capabilities work, most don't. Check what the passing tests have in common (e.g., no cluster creation)
- `never_failing_count` > 0 → some tests always pass, invisible to failure analysis. Note the coverage gap

## Cross-Run-Type Correlation

- **Periodic timeout + presubmit provision failure** → shared infra
- **Same signature in periodic + presubmit** → code bug, not environment
- **Periodic fails, presubmit passes** → environment or deploy issue
- **Presubmit fails, periodic passes** → PR code issue or ephemeral infra
- **Nightly fails with different tests** → OCP candidate regression, not ARO
- **Cascade timeout + `/cidig` shows `lro_classification: accepted_stuck`** → CS layer blocked (deny assignment, CS crash). Check if all EV2 hashes fail — if yes, not a code regression. Check Sippy duration trends for onset timing and rollout gap patterns.
- **Cascade timeout + `/cidig` shows `lro_classification: provisioning_stuck`** → HyperShift/Maestro layer. Use `/cidig` to check OCP version/channel from test output — if `channel=nightly`, likely OCP nightly build issue (recommend checking if stable tests pass). If stable/candidate, environment-specific HyperShift/Maestro issue.
- **Nightly failures only in nightly channel, stable passes in same run** → OCP nightly build issue, not infrastructure. Report the specific nightly build version from test output.

## Command Budget

**periodic:** 1 (summary) + up to 3 (env detail) + up to 2 (nightlies) = 6 max. Typical when 1/3 envs degraded: 3.
**presubmit:** 1. **all:** periodic budget + 1 (presubmit) = 7 max.

## Data Reference (compact format)

### Per-environment data

- **status**: `{streak, current_green, pass_rate, total_runs, baseline, failure_stage}`
  - `baseline`: `{pass_rate, total_runs, days, verdict}` — rolling pass rate from runs BEFORE the current window. `verdict` is "DEGRADED" (>15pp drop), "improved" (>15pp gain), or "normal". Use this to distinguish real regressions from normal variance.
- **data_window**: `{requested_days, actual_days, oldest_run, newest_run, truncated, nightly_runs_excluded, run_count_capped}`
- **daily_rates**: `[{date, pass, total}]`
- **ev2_coverage**: `{with_ev2, total}`
- **ev2_hash_rates**: `[{hash, pass, fail, total, pass_rate, is_cron}]`
- **failure_scale_dist**: `{none, isolated, moderate, cascade}`
- **region_rates**: `[{region, pass, total, pass_rate, low_sample}]`
- **test_suite_size**: Count of distinct tests that have ever failed in the window (lower bound on suite size)
- **never_failing_count**: Tests in the suite that never appear in any failure list (always pass)
- **cascade_survivors**: `[test_name, ...]` — tests that fail in some cascade runs but not all. During cascade failures these tests intermittently survive, revealing which capabilities still work when cluster creation is broken. Empty when <2 cascade runs.
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

## Handoff

After producing the report, write a handoff file so subsequent `/cidig` investigations start with fleet context instead of cold:

```bash
cat > /tmp/ciscan-context.json << 'HANDOFF'
{json}
HANDOFF
```

The JSON structure:
```json
{
  "timestamp": "ISO-8601 of when this scan ran",
  "scope": "periodic|presubmit|all",
  "window_days": 7,
  "environments": {
    "int": {"status": "degraded|healthy|no_data", "pass_rate": 72.7, "baseline_verdict": "DEGRADED"},
    "stg": {"status": "healthy", "pass_rate": 95.0, "baseline_verdict": "normal"}
  },
  "findings": [
    {
      "id": "F1",
      "title": "one-line finding title",
      "severity": "P1|P2|P3",
      "envs": ["int", "stg"],
      "signature_key": "the normalized signature key",
      "annotations": ["cascade", "cross_env:2"],
      "hypothesis": "your hypothesis about root cause",
      "recommended_runs": [
        {"id": "run_id_string", "env": "int", "url": "prow_url", "reason": "why this run"}
      ],
      "what_to_look_for": ["lro_classification", "azure ResponseErrors", "provision failures"]
    }
  ],
  "fleet_context": {
    "cross_env_signatures": ["signature keys that appear in 2+ envs"],
    "all_hashes_failing": {"int": true, "stg": false},
    "nightly_correlation": "description if nightly shows same pattern, or null"
  }
}
```

**Rules:**
- One finding per distinct root cause hypothesis (merge related signatures)
- `recommended_runs`: pick the best run per finding — highest failure count on the most recent hash, or the run with the clearest signal
- `what_to_look_for`: list the specific triage fields and artifact types that would confirm or deny the hypothesis
- `hypothesis`: be specific ("CS layer blocked by deny assignment" not "infrastructure issue")
- Always write the file, even if no findings (empty `findings` array = fleet healthy)
