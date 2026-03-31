# CI Triage Tools

Data acquisition tooling for triaging ARO-HCP Prow CI e2e test failures. Handles GCS HTML parsing, parallel job status fetching, junit XML extraction, and presubmit path indirection — the parts that are unreliable when done ad-hoc.

Analysis is left to the agent or human. The script acquires data; it does not classify, correlate, or recommend.

See [ADR.md](ADR.md) for design rationale and validation data.

## Usage

In Claude Code: `/triage int`, `/triage pr 4618`

Standalone:
```bash
# Environment health (start here)
python3 hack/ci-triage/prow.py env-health int periodic

# List recent jobs
python3 hack/ci-triage/prow.py list-jobs stg periodic --failed

# Per-test failures from a specific job
python3 hack/ci-triage/prow.py fetch-failures BASE_URL int

# CI step failures (when tests didn't run)
python3 hack/ci-triage/prow.py fetch-step-failures BASE_URL

# Build logs (test runner or provisioning step)
python3 hack/ci-triage/prow.py fetch-build-log BASE_URL int
python3 hack/ci-triage/prow.py fetch-build-log BASE_URL int --step provision

# All periodic jobs (e2e + cleanup + global)
python3 hack/ci-triage/prow.py all-periodic int

# Find a job by ID (searches all envs/types)
python3 hack/ci-triage/prow.py lookup-job 2038890153002405888

# PR check status
python3 hack/ci-triage/prow.py pr-checks 4618

# Presubmit path resolution
python3 hack/ci-triage/prow.py resolve-url JOB_ID int

# GCS directory listing
python3 hack/ci-triage/prow.py list-dir URL
```

All commands output JSON. Commands that accept a base URL also accept Prow dashboard URLs (`prow.ci.openshift.org/view/gs/...`).

## Components

- `prow.py` — Data acquisition script (10 commands, 0 external dependencies)
- `ENDPOINTS.md` — Artifact path reference for manual deep-dives

## Prerequisites

- Python 3.9+
- `gh` CLI (optional) — `pr-checks` uses it when available, falls back to GitHub REST API for public repos

## Tests

```bash
python3 -m unittest discover -s hack/ci-triage/tests
```
