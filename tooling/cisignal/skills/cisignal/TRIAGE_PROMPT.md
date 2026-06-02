You are triaging CI failures for the {env} environment.

The triage-input data is included below. Pattern groups are
pre-grouped by the tool. Your job: merge related groups, classify
each, and explain the root cause.

## Merging

- Timeout variants affecting the same operation are one failure mode.
- "Interrupted by User" = cascade. Cleanup failure after a
  creation timeout = cascade. Fold under root cause.
- Same error pattern across tests = shared root cause.
- Check pass rates. 0% for 14 days = chronic.

## Classification

| Signal | Meaning |
|--------|---------|
| pass_rate near 0%, sustained | Chronic |
| pass_rate high, many recent hits | Regression |
| pass_rate 50-90%, intermittent | Flake |
| Pre > 0, Per = 0 | PR-specific failure (not on main) |
| Pre = 0, Per > 0 | Broken on main |
| Pre > 0, Per > 0 | Confirmed regression on main |
| Many tests share same PRs column | Single PR causing cascade |
| pool_retries > 0 | Identity pool contention |
| pool_retries = 0, timeout on cluster creation | Azure slow |
| "not allowed", "invalid", "rejected" | API rejection |
| "rate limiter", "connection reset" | CI infra |

Classify by the innermost cause, not the outermost wrapper.
Never lump more than 5 unrelated tests into one FM — split further
until each FM has a coherent error pattern or shared root cause.

When multiple tests share the same PRs, group them as one FM
caused by that PR — don't list each test as a separate failure.

## Rules

- One section per failure mode, not per test.
- >5 tests: describe the category. <=5: name them.
- Cite signals (pool retries, pass rate), not lists.
- When citing errors, preserve the specific resource or deployment name from the error text.

## Output

Return triage as text (do NOT write files):

## FM{n}: {short label}
- **Type:** root cause | cascade | isolated
- **Impact:** {count} hits, {n} runs, {n} tests
- **Window:** {first_seen} -> {last_seen}
- **Root cause:** {what failed and why — cite error text}
