---
name: cisignal
description: "Investigate CI fleet failures — triage and report"
---

# CI Signal

Produce a CI signal report — what's broken, why, and what to do.

```
/cisignal [SCOPE] [--days=N]
```

- No arguments — all four environments (int, stg, prod, dev).
- `dev` — DEV only (presubmit + periodic).
- `periodic` — INT, STG, PROD only (these envs are periodic-only).
- An environment name — single env (accept aliases: stage->stg).

**Note:** INT *deployment* runs via ADO/EV2, but the e2e tests
against INT run as Prow branch jobs (`branch-ci-...-integration-
e2e-parallel`). Cisignal sees these via Sippy.

## Phase 0: Build and collect

**Goal:** Fetch fleet data for every environment in scope.

```bash
cd tooling/cisignal && GOWORK=off go build -o /tmp/cisignal ./cmd/cisignal && cd -
```

Rebuild every run. Run for each environment in scope.

Write fleet output to temp files — one per environment:

```bash
/tmp/cisignal fleet --env=$env --days={days} > /tmp/cisignal-$env.md
```

After collecting all environments, read each file's **header and
table only** (everything before `## Errors`) into main context.

Also run ops and changes checks (once each, not per environment):

```bash
/tmp/cisignal ops --days={days}
gh pr list --repo Azure/ARO-HCP --state merged --search "merged:>=$(date -d '{days} days ago' +%Y-%m-%d)" --limit 30 --json number,title,mergedAt,author,files
```

If a fleet command fails or produces empty output, note the
environment as "data unavailable" in the report header table
and skip it in Phase 1. Do not treat missing data as "no failures."

### Understanding the tool output

The fleet command outputs a flat table of all failing tests sorted
by pass rate (worst first), followed by deduplicated error samples
and Prow run URLs for each test.

Key columns:
- `Pre`: presubmit (PR-triggered) hit count. Each hit = one Prow
  run where this test failed (deduplicated per run).
- `Per`: periodic hit count (same semantics).
- `PRs`: unique PR numbers associated with presubmit hits. Empty
  for periodic-only failures. Many tests hitting the same PR =
  one root cause.
- `Pass%`: 14-day historical pass rate, not the current window.
- `Pool`: identity container acquisition retries / wait time.

Interpreting Pre vs Per:
- Pre-only (Per=0): failure specific to PR(s), not on main.
- Per-only (Pre=0): broken on main, no PR to blame.
- Both > 0: confirmed regression on main, PRs re-confirming it.

Error dedup: The tool collapses structurally identical errors by
stripping instance identifiers (UUIDs, resource group names,
cluster names, timestamps, OCP versions) for comparison only —
the raw error text is preserved in the output. Each distinct
error shape shows one raw sample, its occurrence count `(xN)`,
and a Prow run link.

## Phase 1: Triage — one per environment

**Goal:** Group failing tests into failure modes and classify each.

Skip environments with zero failing tests.

**Small environments (<20 failing tests):** Read the full file
(`/tmp/cisignal-$env.md`) into main context and triage inline
following `TRIAGE_PROMPT.md`.

**Large environments (20+ failing tests):** Spawn a Haiku agent
to triage. The agent reads the full file, groups failure modes,
and returns a compact triage summary. Haiku does classification
and grouping — not root cause reasoning.

For each large environment, use:
- `description`: `"Triage {env} CI failures"`
- `model`: `"haiku"`
- `prompt`: read `TRIAGE_PROMPT.md` (in this skill directory),
  substitute `{env}`, and tell the agent to read the fleet
  output from `/tmp/cisignal-{env}.md`.

Send all agent calls in a single message so they run
concurrently. Agents must NOT write files.

## Phase 2: Report — synthesize across environments

**Goal:** Merge all triage into one ranked, cross-env report.

This phase runs in the main context (Opus). The main context has:
- The table + header from each environment (from Phase 0)
- Triage results (inline for small envs, agent returns for large)
- Ops and changes data

**Cross-env:** Compare failure modes across environments. Same
error in multiple envs = shared root cause. Failures in one env
only = env-specific (deploy, config, capacity).

**Recent changes:** Use the changes output from Phase 0. Check if
any PR's merge time overlaps with a failure mode's onset window and
whether the changed files plausibly relate. Cite the PR if so.

**Ops:** Use the ops output from Phase 0. Correlate cleanup/infra
job failures with environment health — failing cleanup accumulates
orphaned resources. Cite the error text.

**Rules:**
- Lead with the worst problem. Priority = impact x confidence.
- Cross-env dedup: same error or affected tests = one entry.
- Cascades belong under the root cause, not as top-level entries.
- Every cause cites evidence. Stop where the data stops.
- No tool-internal field names. Plain language.

**Self-check before writing:** Count the FMs from each
environment's triage. Verify the report accounts for every one —
either as its own entry or explicitly folded under another. If any
FM is unaccounted for, add it before writing.

**Output:** Print the report directly — no file unless the user
asks for one.

```markdown
# CI Signal — {scope} — {date_range}

| Env | Pass Rate | Runs | Streak |
|-----|-----------|------|--------|

## Failure modes

### 1. {short label} ({envs affected})

**Error**: `{one-line error message}`

**Impact**: {count} failures across {n} runs, {n} tests affected
- Window: {first_seen} -> {last_seen}
- Status: {active | resolved}
- Sample runs: {1-2 Prow run URLs}

**Cause**: {what broke and why — cite the error}.

**Cascades**: {labels of cascade victims, or omit}

**Action**: {what to do next — or "no action, resolved"}

---
```
