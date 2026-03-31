# ADR: ARO-HCP CI Triage Tooling

**Status:** ADOPT
**Date:** 2026-03-31

## Context

Triaging Prow CI e2e failures requires navigating GCS artifact directories, parsing HTML listings, following presubmit path indirection, and reading junit XML — before any analysis begins. This takes 15-30 minutes per pass. The Prow dashboard is JS-rendered (unusable via fetch), and the prowjobs API is a 169MB firehose. An un-augmented agent can reach test-level data for a single job in ~90 seconds; it cannot do cross-environment comparison at all.

## Decision

Add `hack/ci-triage/prow.py` — a data acquisition tool that handles GCS parsing, parallel HTTP fetching, junit extraction, and GitHub API access. 10 CLI commands, zero dependencies beyond Python stdlib. Analysis is left to the agent or human.

## Consequences

- Cross-environment health checks (60 jobs, 3 envs) complete in ~10 seconds
- Failure pattern detection (flake vs. systemic) works from structured data instead of manual browsing
- PR check classification covers both Prow commit statuses and GitHub Actions check-runs with flake/resolved detection
- The tool encapsulates GCS path conventions, container names, and presubmit indirection that would otherwise need to be rediscovered each session

## Risks

- Job name constants (`PERIODIC_JOBS`, `PRESUBMIT_JOBS`, `TEST_STEPS`) are hardcoded. New job types require a code change.
- `env-health` defaults (20 jobs, 5 samples) are sufficient for current failure rates. At higher rates or with more environments, defaults may need tuning.
- `pr-checks` via GitHub REST API is subject to rate limits on unauthenticated requests (60/hour). The `gh` CLI path avoids this.

## Validation

153 unit tests (offline via MockFetcher). Live-tested against int, stg, and prod periodic jobs and PR checks (2026-03-31).
