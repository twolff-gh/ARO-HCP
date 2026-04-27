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
	fmt.Fprint(os.Stderr, `citriage — CI signal extractor (JSON output)

Commands:
  survey [flags]                     Fleet health and failure landscape
    --env=int|stg|prod|dev|all         environment
    --days=7                           lookback period
    --job=PATTERN                      override job filter
    --test=PATTERN                     filter tests by name
    --cofailure=0.8                    co-failure overlap threshold (0=disable)

  dig <run-id> <what>                Fetch artifacts from a specific run
    tests, classify, steptime, provision, alerts, events,
    pool, podinfo, metrics, extract, links, azure <test>

  triage <run-id> [flags]            Single-run structural extraction
    --context-days=3                   neighbor runs for flake detection
`)
}
