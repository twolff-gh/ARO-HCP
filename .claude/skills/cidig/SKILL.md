---
name: cidig
description: "Investigate CI failures — evidence-based root cause analysis"
---

# CI Investigation — Root Cause Analysis

You investigate CI failures by examining artifacts from a specific run, following evidence to root cause or an explicit inconclusive finding.

## Routing

1. Have a specific run ID or Prow URL → `/cidig` (you are here)
2. Have a signal group from a prior `/ciscan` report → `/cidig` (you are here)
3. Have a specific test name, error message, or PR number → `/cidig` (you are here)
4. Everything else ("how is CI?", "fleet status", no specific target) → `/ciscan`

## Setup

```bash
go build -o /tmp/citriage ./tooling/citriage/
```

All output is JSON.

## Starting an Investigation

### Primary entry: `triage` (always start here)

```bash
/tmp/citriage triage <run-id> [--context-days=3] [--baseline=<passing-run-id>]
```

The `triage` command extracts structural signals from ALL artifacts in a single call. It returns a compact JSON object (~16KB for a cascade failure, ~8KB for an isolated failure) containing:

- **scale** — failure count, error group count, largest group percentage
- **context** — environment, EV2 hash, region, timeout threshold (from prowjob.json)
- **errors** — deduplicated error groups with test counts, source file:line, innermost cause
- **steps** — pipeline step durations with failed flags and exit codes
- **metrics** — ci-operator events (level, success, cause field), lease/pod latency
- **build_log** — ERROR/FATAL lines, step result lines, tail, test fail count
- **podinfo** — exit code, reason, OOM flag
- **events** — CiJobFailed presence, warning count
- **pool** — MSI container pool contention signals
- **provision** — presubmit pipeline step results (total/failed counts)
- **alerts** — presubmit Azure Monitor alerts (name, severity, known-issue flag)
- **azure** — per-test ResponseError codes
- **links** — test-to-resource-group mapping from custom-link-tools
- **neighbors** — neighboring runs with same EV2 hash (pass/fail rates, per-test consistency)

**Read the triage output first.** It tells you what kind of failure this is and where to look next. The `errors` field groups failures by similarity — when `largest_group_pct` is high (e.g. 70%+) with many failures, that typically means one root cause, not N independent issues.

### With context from ciscan

Use the handoff — don't re-run `survey`. Start with `triage <run-id>` using the run ID from the ciscan finding.

### Cold start with test name (no run ID)

Use `survey --env=<env> --test=<pattern> --days=3` to find recent failing runs, then `triage <best-run-id>`.

## Second-Pass Drill-Down

After `triage` gives the structural picture, use individual `dig` subcommands to drill into specific artifacts that need deeper inspection. These are for when the triage output points you somewhere specific — NOT for every investigation.

### `dig <run> azure <test>`
Full Azure API call trace for a specific test — ALL entries with time, level, event, message. Use when triage shows ResponseError codes and you need the full API sequence.

### `dig <run> tests`
Full test result list (all tests, not just failed). Use when you need to see passing test durations for comparison, or when the triage error grouping needs verification.

### `dig <run> metrics`
Full ci-operator metrics JSON. Use when triage `metrics` events point to infrastructure issues and you need the complete lease/pod/node data.

### `dig <run> provision`
Presubmit only. Full ARM deployment pipeline steps (~133) with per-step pass/fail and failure messages. Use when triage `provision` shows failed steps and you need the full ARM error chain.

### `dig <run> alerts`
Presubmit only. Full Azure Monitor alert details. Use when triage `alerts` shows unknown alerts worth investigating.

### `dig <run> classify`
Error grouping and cascade detection without the full triage extraction. Use when you only need the error structure (e.g., comparing two runs).

### `dig <run> steptime [baseline-run-id]`
Step-graph durations with optional baseline comparison. Use when triage `steps` show unusual durations and you want to compare against a known-good run.

### `dig <run> extract`
Build-log + metrics structural extraction. Use when you need the extracted signals without the full triage (e.g., for a passing run comparison).

### `dig <run> links`
Test-to-resource-group mapping from custom-link-tools HTML. Use when you need Kusto query links or RG names for Azure portal investigation.

### `dig <run> events`
K8s events from the build cluster. Use when triage `events` shows warnings worth inspecting.

### `dig <run> podinfo`
Full pod info with container statuses. Use when triage `podinfo` shows OOM or unexpected exit codes.

### `dig <run> pool`
MSI container pool state. Use when triage `pool` shows contention signals.

## Investigation Flow

```
1. triage <run-id>                    ← structural overview (always)
   ├── Read scale: cascade? isolated?
   ├── Read errors: what failed?
   ├── Read steps: which step, how long?
   └── Read neighbors: known pattern? flake?

2. Drill into specifics (0-3 calls based on triage findings):
   ├── dig azure <test>               ← when ResponseErrors present
   ├── dig provision                  ← when presubmit provision failed
   └── dig metrics                    ← when infra metrics need detail
```

Most investigations need only steps 1-2 (2-4 total commands).

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

### Key caveats
- **Prow timeout corruption:** When `startTime == endTime` in test results, Prow killed the test. Duration data is garbage.
- **Cascade failures:** When `largest_group_pct` is high (e.g. 70%+) with many failures, the dominant error group likely shares one root cause. Investigate that group, not individual tests.
- **EV2 annotations** are stronger deploy correlation evidence than GitHub PRs. Only on periodic runs. Presubmit has PR number/SHA instead.
- **Synthetic tests** (`[sig-sippy]*`, `Job run should complete before timeout`) are filtered automatically.
- **Azure ResponseErrors for timeouts:** The dominant failure mode (cluster creation timeout) produces zero Azure API errors. Triage's `azure` field will be empty. This is correct — the timeout is client-side, not an API failure.
- **Pool state is end-of-run snapshot.** Contention signals in `pool` reflect final state, not peak contention. Build-log error lines capture the temporal contention signal.

### Rule-out using triage fields

Check these fields directly — null means the artifact wasn't available:
- `podinfo.oom_detected` → OOM kill
- `pool.contention` → MSI pool exhaustion
- `events.ci_job_failed` → CI infrastructure failure
- `metrics.max_lease_acquisition_seconds` → Boskos pool exhaustion
- `provision.failed_steps` → ARM deployment issues (presubmit)
- `neighbors.same_hash_passed > 0` → same deploy passes sometimes → flake
- `neighbors.same_hash_passed == 0` → consistent failure → regression candidate

## Command Budget

Maximum **8 commands** per investigation. The `triage` call counts as 1. Typical investigation: 2-4 commands total.

## Output

Investigations end in one of two outcomes:

### ROOT CAUSE FOUND

```
## Root Cause: [one-line summary]

**WHAT:** [specific mechanism — not "tests are failing" but what is actually broken]
**WHEN:** [onset date/time, from triage neighbors or survey]
**WHERE:** [layer: Azure API | ARM deployments | pool/scheduling | platform | test code | deploy change]
**WHY:** [trigger — deploy change, config change, resource exhaustion, upstream issue]
**Error file:** [source_file.go:line from the triage error group]

### Evidence Chain
- [fact]: [data from triage field] — [URL]
- [fact]: [data from dig drill-down] — [URL]

### Triage Summary
- Scale: [failed_test_count]/[total_test_count], largest_group=[largest_group_pct]%
- Error groups: [count] ([dominant group test count] tests share dominant signature)
- Coverage: [list has_* fields present] | OOM=[bool], pool=[bool], azure_errors=[N], lease=[N]s

### Cross-Run Confirmation
- [neighbors context or search results, or "skipped — one-off"]
```

### INCONCLUSIVE

```
## Inconclusive: [one-line symptom summary]

**WHAT:** [symptom observed]
**WHEN:** [onset with evidence]
**WHERE:** Beyond CI artifact visibility — suspected [platform layer]
**Error file:** [source_file.go:line]
**RULED OUT:** [causes eliminated, citing triage fields as evidence]

### Triage Summary
- Scale: [failed/total], cascade=[bool]
- Coverage: [list has_* fields present] | OOM=[bool], pool=[bool], azure_errors=[N], lease=[N]s

### Next Step
[What investigation is needed — Kusto query, service logs, etc.]
- Resource group: [from triage links field]
- Time window: [from triage timestamp + duration]
- Kusto links: [from triage links field if available]
```
