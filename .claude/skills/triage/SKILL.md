---
name: triage
description: Triage Prow CI e2e test failures for ARO-HCP environments
argument-hint: <env|pr> [type|pr-number]
user-invocable: true
---

Triage Prow CI failures for ARO-HCP.

## Arguments

`$ARGUMENTS` is one of:
- `ENV` or `ENV TYPE` — environment health triage (e.g., `int`, `stg periodic`, `dev presubmit`)
- `pr PR_NUMBER` — PR-specific triage (e.g., `pr 4618`)

Defaults: TYPE defaults to `periodic` for int/stg/prod, `presubmit` for dev (dev has no periodic jobs).

## Tools

All commands output JSON. Run from repo root.

- `python3 hack/ci-triage/prow.py env-health ENV TYPE` — Pass/fail ratio + failure samples. **Start here for env triage.** Also run `all-periodic` to check non-e2e jobs.
- `python3 hack/ci-triage/prow.py list-jobs ENV TYPE [--failed]` — Recent jobs with state, timestamps, base URLs.
- `python3 hack/ci-triage/prow.py fetch-failures BASE_URL ENV` — Per-test failures from junit.xml.
- `python3 hack/ci-triage/prow.py fetch-step-failures BASE_URL` — CI step-level failures (fallback when no junit).
- `python3 hack/ci-triage/prow.py fetch-build-log BASE_URL ENV [--step provision] [--lines N]` — Test runner or provisioning build-log.txt (tail). Use `--step provision` for ARM errors when tests didn't run.
- `python3 hack/ci-triage/prow.py resolve-url JOB_ID ENV` — Presubmit GCS path indirection.
- `python3 hack/ci-triage/prow.py pr-checks PR_NUMBER` — Currently-failing GitHub checks (uses `gh` CLI if available, falls back to GitHub API).
- `python3 hack/ci-triage/prow.py all-periodic ENV` — Latest status of all periodic jobs (e2e, cleanup, global). Use to check for broken non-e2e jobs.
- `python3 hack/ci-triage/prow.py lookup-job JOB_ID` — Find a job by ID across all envs and types. Returns env, type, state, base_url.
- `python3 hack/ci-triage/prow.py list-dir URL` — GCS directory listing.

Commands that accept `BASE_URL` also accept Prow dashboard URLs (`prow.ci.openshift.org/view/gs/...`) — they are converted automatically.

When `fetch-failures` returns `no_junit`, tests didn't run — use `fetch-step-failures` to see which CI step failed, then `fetch-build-log --step provision` for the full provisioning error.

Artifact path reference: `hack/ci-triage/ENDPOINTS.md`
