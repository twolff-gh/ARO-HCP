---
name: cidig
description: "Investigate CI failures — evidence-based root cause analysis"
---

# CI Investigation — Root Cause Analysis

You investigate CI failures by examining artifacts from a specific run, following evidence to root cause or an explicit inconclusive finding.

## Routing

1. Need per-test Azure API traces, ARM operation timing, or test output context → `/cidig` (you are here)
2. Have a specific run ID from a `/ciscan` finding → `/cidig` (you are here)
3. Everything else ("how is CI?", "fleet status", no specific target) → `/ciscan`

**Note:** `/ciscan` now provides per-run envelopes (exit codes, error chains, step timings, lease waits, alerts, provision failures) and pre-grouped signatures. Use `/cidig` only when you need PER-TEST depth that envelopes don't provide — Azure API call sequences, ARM operation trees, test output context for "Interrupted by User" errors.

## Setup

```bash
go build -o /tmp/arohcp-ci-triage ./tooling/arohcp-ci-triage/
```

All output is JSON.

## Starting an Investigation

### Primary entry: `triage` (always start here)

```bash
/tmp/arohcp-ci-triage triage <run-id> [--context-days=3] [--baseline=<passing-run-id>]
```

One call extracts structural signals from ALL artifacts. Returns a JSON object:

| Field | Source | What it tells you |
|-------|--------|-------------------|
| `total_tests` / `failed_tests` | extension_test_result | Scale of failure |
| `failures[]` | extension_test_result | Each failed test: `name`, `duration_seconds`, `error`, `output_tail` (last N lines of stdout) |
| `error_groups[]` | normalized failures | Deduplicated signatures with counts (only when >5 failures) |
| `context` | Sippy + run metadata | `env`, `is_presubmit`, `ev2_hash`, `region`, `pull_number` |
| `steps[]` | ci-operator-step-graph | Pipeline step durations, which failed |
| `metrics` | ci-operator-metrics | Step events, lease acquisition, pod scheduling latency |
| `build_log` | build-log.txt | ERROR/FATAL lines, step results, tail, test fail count |
| `podinfo` | podinfo.json | Exit code, reason, `oom_detected` |
| `events` | build-resources/events.json | `ci_job_failed`, warning count |
| `pool` | identities-pool-state.yaml | MSI container counts, `contention[]` |
| `links[]` | custom-link-tools HTML | Test → resource group → Kusto cluster mapping |
| `neighbors` | Sippy (same-hash runs) | Pass/fail rates, per-test consistency |
| `provision` | junit_entrypoint.xml | Presubmit only: pipeline step pass/fail |
| `alerts[]` | alerts.json | Presubmit only: Azure Monitor alerts |
| `azure[]` | azure.log per test | ResponseError codes, `lro_states` (Accepted/Provisioning/Running/Succeeded counts), `lro_poller_types` (which Azure resource) |
| `lro_classification` | derived from azure[] | `accepted_stuck` (CS blocked), `provisioning_stuck` (HyperShift/Maestro), or empty (healthy/no data) |

### With context from ciscan

Check for handoff state before starting the investigation:

```bash
cat /tmp/ciscan-context.json 2>/dev/null
```

**If the file exists and `timestamp` is within the last hour:**

1. **Find the matching finding** — match the run ID against `findings[].recommended_runs[].id`. If matched, you have:
   - `hypothesis`: what ciscan thinks is wrong — your job is to confirm or deny it
   - `what_to_look_for`: which triage fields to prioritize
   - `annotations`: fleet-level structural observations (cascade, cross_env, etc.)
   - `fleet_context`: cross-env and nightly correlation already computed

2. **Skip redundant work:**
   - If `fleet_context.all_hashes_failing[env]` is true → note "not deploy-correlated" without re-verifying via neighbors
   - If `fleet_context.cross_env_signatures` contains this run's error → note "fleet-wide" and focus on mechanism
   - If `fleet_context.nightly_correlation` is set → factor into root cause assessment
   - If the finding has `annotations` including `cascade` → expect >15 failures, skip counting

3. **Test the hypothesis** — run `triage <run-id>` and follow the decision tree for the failure mode suggested by the hypothesis, not the generic flow. Report as CONFIRMED, DENIED, or INCONCLUSIVE with evidence.

4. **Reference the finding ID** (e.g., "F1") in your output for traceability back to the ciscan report.

**If the file is missing, stale, or the run ID isn't in any finding** — proceed with normal cold-start investigation below.

### Cold start with test name (no run ID)

Use `survey --env=<env> --test=<pattern> --days=3` to find recent failing runs, then `triage <best-run-id>`.

## Failure Mode Decision Trees

Read the triage output. Classify the failure mode, then follow the appropriate tree:

### Mode A: Timeout (duration ~2700s, error "context deadline exceeded")

```
1. podinfo.oom_detected?        → YES: OOM kill. Report memory pressure.
2. pool.contention non-empty?   → YES: Pool exhaustion caused timeout. Report pool state.
3. metrics.max_lease_acq > 300? → YES: Boskos exhaustion. Report lease wait.
4. steps: which step is longest?
   └─ provision step?           → YES: Infra provisioning timeout.
   └─ test step at full timeout? → Continue to 5.
5. azure[] has ResponseErrors?  → YES: Azure API issue. Follow Mode B.
                                → NO: Continue to 5a.
5a. lro_classification field?
   └─ "accepted_stuck"          → CS layer blocked. Cluster creation PUT was accepted
                                  by ARM but CS never started provisioning. Root cause
                                  is in CS (deny assignment failure, CS crash, config error).
                                  Cross-ref EV2 hash rates: all hashes failing = not code.
                                  Check Sippy duration trends for onset timing.
   └─ "provisioning_stuck"     → HyperShift/Maestro layer. CS started provisioning
                                  but cluster never became Ready. Check:
                                  - azure[].lro_states: high Provisioning count confirms stall
                                  - Test output for ocp_version/ocp_channel: if channel is
                                    "nightly", likely OCP nightly build issue (check if stable
                                    channel tests pass in the same or concurrent runs).
                                    If channel is "stable" or "candidate", environment-specific
                                    HyperShift/Maestro issue.
                                  - For presubmit: check provision failures for HyperShift
                                    Operator deployment state.
   └─ "" (empty)               → Not enough azure.log data. Fall back to:
                                  neighbors.same_hash_passed? >0: flake. 0: consistent.
6. neighbors.same_hash_passed?  → >0: Intermittent (flake). 0: Consistent.
```

**LRO sub-classification** is the strongest diagnostic signal for cluster creation timeouts. The `azure[]` array now includes `lro_states` (map of state→count) and `lro_poller_types` (which Azure resource each LRO operates on). Key patterns:

| lro_classification | LRO Pattern | Root Cause Layer |
|-------------------|------------|-----------------|
| `accepted_stuck` | Accepted=258, Provisioning=0 | CS layer (deny assignment, crash, ARM rejection) |
| `provisioning_stuck` | Accepted=28, Provisioning=232 | HyperShift/Maestro/mgmt cluster |
| (empty, healthy) | Accepted=14, Provisioning=84, Succeeded=4 | Normal lifecycle |

For cascade runs (>15 failures), azure.log is sampled from the first 3 failing tests. The `lro_classification` is derived from these samples.

### Mode B: Azure API Error (azure field has ResponseErrors)

```
1. Read error codes in azure[].response_errors
2. build_log.error_lines has same error? → Confirms propagation
3. provision (presubmit): same error?    → Infra setup failure, not test
4. Common codes:
   ResourceNotFound    → Stale reference or race condition
   QuotaExceeded       → Capacity (check region)
   AuthorizationFailed → RBAC misconfiguration
   RoleAssignment*     → MSI/permission issue
```

### Mode C: Pipeline Step Failure (step in steps[] has failed=true)

```
1. Which step failed? Check steps[].name + failed=true
2. build_log.step_lines → step result message
3. steps[].error_snippet → JUnit error for the step
4. metrics.events → cause field for the step
5. provision.failures (presubmit) → full ARM error chain
```

### Mode D: Infrastructure (events.ci_job_failed=true)

```
1. This is CI platform failure, not test failure
2. build_log.error_lines → CI error details
3. metrics → lease/pod scheduling issues
4. podinfo → node eviction, scheduling failure
5. Usually transient — check neighbors to confirm
```

### Mode E: Cascade (failed_tests >> 5, same error)

```
1. Read error_groups[] — errors are already deduplicated by normalized signature.
   Each group is one mechanism. Don't manually cluster.
2. If 1 group → one root cause. Follow the mode for that error type.
3. The test count is irrelevant — report the single mechanism
4. Shortest-duration failure = earliest failure = likely trigger test
```

## Artifact-to-Root-Cause Mapping

| Field | Contains ROOT CAUSE for | Contains SYMPTOM for |
|-------|------------------------|---------------------|
| `failures[].error` | Test-level bugs, assertion failures | Infra issues (shows "timeout" not why) |
| `steps` | Pipeline/build issues | — |
| `azure` | Azure API errors, throttling | — |
| `provision` | ARM deployment failures (presubmit) | — |
| `metrics` | Scheduling/lease issues | — |
| `podinfo` | OOM kills, node eviction | — |
| `pool` | Pool exhaustion | — |
| `events` | — | CiJobFailed = something broke, not what |
| `build_log` | — | Error lines are downstream effects |
| `neighbors` | — | Statistical context (flake vs regression) |

## Signal Absence as Evidence

Null/empty fields are diagnostic:
- `output_tail` empty → either cascade budget (>20 failures) or test had no stdout
- `error_groups` empty → fewer than 6 failures, each error is unique
- `azure` empty + timeout → client-side timeout, not API failure (the dominant mode)
- `pool.contention` empty → not pool exhaustion
- `podinfo.oom_detected` false → not memory pressure
- `events.ci_job_failed` false → test failure, not CI infra
- `metrics.max_lease_acquisition_seconds` near 0 → not Boskos contention
- `provision` null (periodic run) → expected. Periodic doesn't provision — but presubmit DOES. When periodic has invisible-layer timeouts, check presubmit provision logs for the same time window.
- `neighbors` null → dev environment or env lookup failed
- `neighbors.same_hash_passed > 0` → flake, not deterministic
- `neighbors.same_hash_passed == 0` → every run on this deploy failed → regression candidate
- All fields present, no errors anywhere → test logic bug, not infra

## Second-Pass Drill-Down

After `triage` gives the structural picture, use `dig` subcommands for deeper inspection. Only when triage points you somewhere specific.

### `dig <run> azure <test>`
Full Azure API call trace — ALL entries with time, level, event, message. Use when triage shows ResponseError codes and you need the full API sequence.

### `dig <run> tests`
Full test result list (all tests, not just failed). Use when you need passing test durations for comparison.

### `dig <run> metrics`
Full ci-operator metrics JSON. Use when triage `metrics` events point to infrastructure issues.

### `dig <run> provision`
Presubmit only. Full ARM deployment pipeline steps (~133) with per-step pass/fail and failure messages.

### `dig <run> alerts`
Presubmit only. Full Azure Monitor alert details.

### `dig <run> steptime [baseline-run-id]`
Step-graph durations with optional baseline comparison.

### `dig <run> events`
K8s events from the build cluster.

### `dig <run> podinfo`
Full pod info with container statuses.

### `dig <run> pool`
MSI container pool state.

### `dig <run> links`
Test-to-resource-group mapping. Use for Kusto queries or Azure portal navigation.

## Domain Knowledge

### Infrastructure dependency chain
```
Regional Pipeline → EventGrid (MQTT) → SVC Pipeline → Maestro Server
  → MGMT Pipeline → Maestro Consumer → HyperShift Operator → HCP → cluster Ready
```

### Three CI run types
- **Ephemeral presubmit** (`pull-ci-*-e2e-parallel`): provisions own infra, has `provision`/`alerts` artifacts
- **Shared-env presubmit** (`pull-ci-*-{stage,integration,prod}-e2e-parallel`): tests unmerged PR code against live environments
- **Periodic** (`periodic-ci-*`): tests deployed code against shared environments. Has EV2 annotations.

### Cross-run-type correlation
When periodic shows all-timeout + zero-Azure-errors (Mode A, step 5-6):
- The root cause is in an invisible layer (EventGrid, Maestro, HyperShift)
- Check presubmit runs from the same time window — they have provision logs
- `dig <presubmit-run> provision` may show EventGrid/Maestro/regional deploy failures
- This correlation (periodic symptom → presubmit root cause) resolves ~30% of otherwise-inconclusive investigations

### Key caveats
- **Prow timeout corruption:** When `startTime == endTime` in test results, Prow killed the test. Duration data is garbage.
- **Cascade failures:** Read the error, don't count the tests. 30 tests sharing one error = 1 problem.
- **EV2 annotations** are stronger deploy correlation evidence than GitHub PRs. Only on periodic runs.
- **Synthetic tests** (`[sig-sippy]*`, `Job run should complete before timeout`) are filtered automatically.
- **Pool state is end-of-run snapshot.** Not peak contention.

### Investigation escalation
1. **Triage artifacts** — test failures, steps, metrics, podinfo, events, pool (you are here)
2. **Azure API trace** — `dig azure <test>` for full request/response sequence
3. **Cross-run-type** — find presubmit run from same time window, check provision logs
4. **External telemetry** — Kusto queries using resource groups from `links`, Grafana dashboards
5. **Service team** — when all CI artifacts exhausted

## Command Budget

Maximum **8 commands** per investigation. The `triage` call counts as 1. Typical: 2-4 commands.

## Output

### ROOT CAUSE FOUND

```
## Root Cause: [one-line summary]

**WHAT:** [specific mechanism]
**WHEN:** [onset from neighbors or survey]
**WHERE:** [layer: Azure API | ARM | pool/scheduling | platform | test code | deploy]
**WHY:** [trigger]

### Evidence Chain
- [fact]: [data from triage field] — [URL]
- [fact]: [data from dig drill-down] — [URL]

### Triage Summary
- Tests: [failed_tests]/[total_tests]
- Distinct errors: [N patterns in failures[].error]
- Infra: OOM=[bool], pool=[bool], azure_errors=[N], lease=[N]s

### Cross-Run
- [neighbors context, or "skipped — one-off"]
```

### INCONCLUSIVE

```
## Inconclusive: [symptom summary]

**WHAT:** [symptom]
**WHEN:** [onset]
**WHERE:** Beyond CI artifact visibility — suspected [layer]
**RULED OUT:** [causes, citing triage fields]

### Next Step
[What investigation needed — Kusto query, service logs, presubmit provision check]
- Resource group: [from links]
- Time window: [from timestamp + duration]
```
