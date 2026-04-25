package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}
	var err error
	switch args[0] {
	case "survey":
		err = runSurvey(args[1:])
	case "dig":
		err = runDig(args[1:])
	case "search":
		err = runSearch(args[1:])
	case "diff":
		err = runDiff(args[1:])
	case "triage":
		err = runTriage(args[1:])
	default:
		printUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `citriage — evidence fetcher for iterative CI investigation (JSON output)

Commands:
  survey [flags]                     Fleet health and failure landscape
    --env=int|stg|prod|dev|all         environment (default: int, all=INT+STG+PROD, dev=presubmit e2e)
    --days=7                           lookback period
    --job=PATTERN                      override job name filter
    --test=PATTERN                     filter failing tests by name
    --cofailure=0.8                    co-failure overlap threshold (0=disable)

  dig <run-id> <what> [args...]       Fetch artifacts from a specific run

  search <pattern> [flags]           Cross-run pattern search
    --age=168h                         max age
    --job=PATTERN                      job name filter regex
    --env=int|stg|prod                 environment filter
    --max=5                            max matches per file

  triage <run-id> [flags]             End-to-end single-run structural extraction
    --baseline=RUN_ID                  compare step durations to a baseline
    --context-days=3                   days of neighboring runs for correlation

  diff <commit1> <commit2>           Deployment version diff via GitHub
  diff --run=RUN_ID                  Auto-detect versions from a run

dig shorthands:
  tests            Test results (name, result, duration, error, output excerpt)
  output <test>    Full test stdout for a specific test
  azure <test>     Azure API trace for a test
  metrics          CI operator metrics (leases, pods, nodes, events)
  provision        Provision pipeline step results (presubmit only)
  alerts           Azure Monitor alerts (presubmit only)
  events           K8s warning events
  pool             MSI container pool state (contention detection)
  podinfo          Pod termination reasons, OOM detection, resources
  classify         Error grouping + cascade detection (structural dedup)
  steptime [base]  Step durations with optional baseline comparison
  links            Test-to-resource-group mapping from custom-link-tools
  extract          Build-log + metrics structural extraction
  prefetch         Pre-cache common artifacts for faster subsequent digs
  <raw-path>       Any artifact by relative path
`)
}
