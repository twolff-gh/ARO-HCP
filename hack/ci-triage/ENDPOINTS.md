# CI Triage — Artifact Reference

All paths relative to `base_url` (from `prow.py list-jobs` output or `prow.py resolve-url`).

## Job-level

- `prowjob.json` (JSON) — Job metadata: `status.{state,startTime,completionTime,url}`, `spec.refs.pulls[0].{number,title,author}` (presubmit).
- `artifacts/junit_operator.xml` (XML) — Step-level pass/fail. Each `<testcase name="...">` is a CI step. `<failure>` text has the error. Fallback when `fetch-failures` returns `no_junit` — use `fetch-step-failures`.
- `artifacts/ci-operator.log` (text) — Full ci-operator log. Large — grep only (`error|fail`).
- `artifacts/build-logs/` (dir) — One log file per image build. Use `list-dir` to enumerate.

## Step-level

Test step name is resolved automatically by `fetch-failures` and `fetch-build-log`. For manual artifact access, `TEST_STEPS` in `prow.py` has the mapping.

- `artifacts/{TEST_STEP}/aro-hcp-test-persistent/build-log.txt` (text) — Test runner stdout/stderr (Ginkgo output). Use `fetch-build-log BASE_URL ENV`.
- `artifacts/{TEST_STEP}/aro-hcp-provision-environment/build-log.txt` (text) — Provisioning output — ARM deployment commands and errors. Use `fetch-build-log BASE_URL ENV --step provision`.

## Test artifacts (`artifacts/{TEST_STEP}/aro-hcp-test-persistent/artifacts/`)

**Primary:** Use `prow.py fetch-failures BASE_URL ENV` — it fetches `junit.xml`, resolves the test step name, and returns structured `[{test, message}]`. This is the fastest path to per-test failure details.

- `junit.xml` (XML) — Per-test junit (Ginkgo). Used by `fetch-failures`.
- `extension_test_result*.json` (JSON array) — Per-test results with timing. Use `list-dir` to find.
- `{TestName}/` (dir) — Per-test artifacts (typically `azure.log`). Use `list-dir`.

## Deep-dive artifacts (manual access via `list-dir` or WebFetch)

Not scripted — use when primary artifacts are insufficient.

- `artifacts/ci-operator-metrics.json` — Step-level durations and resource usage. Helps distinguish timeout vs. fast-fail.
- `podinfo.json` — Pod lifecycle events, node placement, OOM kills. Check when infra-level failure suspected.
- `artifacts/{TEST_STEP}/aro-hcp-test-persistent/artifacts/resourcegroups/{TestName}/deployments.yaml` — ARM deployment status per test. Check when Azure resource errors. Use `list-dir` to find test names.
- `artifacts/{TEST_STEP}/aro-hcp-test-persistent/artifacts/identities-pool-state.yaml` — MSI resource pool contention. Check when identity/auth failures cluster.
- `artifacts/build-resources/events.json` — K8s events from CI build namespace. Check when image build or source clone fails.

## Prow URL to job ID

Extract the 19-digit number from any Prow URL. Determine env/type from the job name:
- `periodic-*-integration-e2e-parallel` → int/periodic
- `periodic-*-stage-e2e-parallel` → stg/periodic
- `periodic-*-prod-e2e-parallel` → prod/periodic
- `pull-*-integration-e2e-parallel` → int/presubmit
- `pull-*-stage-e2e-parallel` → stg/presubmit
- `pull-*-prod-e2e-parallel` → prod/presubmit
- `pull-*-e2e-parallel` (no env prefix) → dev/presubmit
