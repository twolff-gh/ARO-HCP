package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
)

// TriageResult is the top-level output of end-to-end single-run structural extraction.
type TriageResult struct {
	RunID       string           `json:"run_id"`
	Job         string           `json:"job"`
	Result      string           `json:"result"`
	ProwURL     string           `json:"prow_url"`
	Timestamp   string           `json:"timestamp"`
	DurationSec float64          `json:"duration_seconds"`
	Scale       FailureScale     `json:"scale"`
	Context     RunContext       `json:"context"`
	Errors      []ErrorGroup     `json:"errors"`
	Steps       []StepTiming     `json:"steps,omitempty"`
	Metrics     *MetricsExtract  `json:"metrics,omitempty"`
	BuildLog    *BuildLogExtract `json:"build_log,omitempty"`
	Links       []TestLink       `json:"links,omitempty"`
	Neighbors   *NeighborContext `json:"neighbors,omitempty"`
	Podinfo     *PodinfoSummary    `json:"podinfo,omitempty"`
	Events      *EventsSummary     `json:"events,omitempty"`
	Pool        *PoolSummary       `json:"pool,omitempty"`
	Provision   *ProvisionSummary  `json:"provision,omitempty"`
	Alerts      []AlertSummary     `json:"alerts,omitempty"`
	Azure       []AzureTestSummary `json:"azure,omitempty"`
}

// RunContext identifies the environment, deploy version, and PR for a run.
type RunContext struct {
	Env            string `json:"env"`
	IsPresubmit    bool   `json:"is_presubmit"`
	EV2Hash        string `json:"ev2_hash,omitempty"`
	Region         string `json:"region,omitempty"`
	PullNumber     int    `json:"pull_number,omitempty"`
}

// NeighborContext summarizes pass/fail rates of nearby runs for cross-run correlation.
type NeighborContext struct {
	WindowDays      int              `json:"window_days"`
	TotalRuns       int              `json:"total_runs"`
	PassedRuns      int              `json:"passed_runs"`
	FailedRuns      int              `json:"failed_runs"`
	SameHashRuns    int              `json:"same_hash_runs,omitempty"`
	SameHashPassed  int              `json:"same_hash_passed,omitempty"`
	SameHashFailed  int              `json:"same_hash_failed,omitempty"`
	TestConsistency []TestFlakeInfo  `json:"test_consistency,omitempty"`
}

// TestFlakeInfo records how often a test failed vs passed across same-hash runs.
type TestFlakeInfo struct {
	TestName   string `json:"test_name"`
	FailedRuns int    `json:"failed_runs"`
	PassedRuns int    `json:"passed_runs"`
	TotalRuns  int    `json:"total_runs"`
}

func runTriage(args []string) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	baseline := fs.String("baseline", "", "baseline run ID for step timing comparison")
	contextDays := fs.Int("context-days", 3, "days of neighboring runs for correlation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return fmt.Errorf("usage: triage <run-id> [--baseline=RUN_ID] [--context-days=3]")
	}
	runID := pos[0]

	dig, err := newDigContext(runID)
	if err != nil {
		return err
	}
	s := dig.summary
	fmt.Fprintf(os.Stderr, "triage: %s | %s | %s\n", runID, s.Name, s.OverallResult)

	result := &TriageResult{
		RunID:       fmt.Sprintf("%d", s.ID),
		Job:         s.Name,
		Result:      s.OverallResult,
		ProwURL:     s.URL,
		Timestamp:   s.StartTime,
		DurationSec: float64(s.DurationSeconds),
	}

	// 1. METADATA
	result.Context = buildRunContext(s)

	// 2. STEP STRUCTURE
	if data, err := dig.store.artifact(dig.base, "artifacts/ci-operator-step-graph.json"); err == nil {
		result.Steps = extractStepTimings(data)
		if jd, err := dig.store.artifact(dig.base, "artifacts/junit_operator.xml"); err == nil {
			enrichStepsWithJUnit(result.Steps, jd)
		}
		if *baseline != "" {
			bs := newSippy()
			if bSummary, err := bs.runSummary(*baseline); err == nil {
				if bBase := gcsBase(bSummary.URL); bBase != "" {
					if bData, err := dig.store.artifact(bBase, "artifacts/ci-operator-step-graph.json"); err == nil {
						applyStepBaseline(result.Steps, bData)
					}
				}
			}
		}
	}

	// 3. ERROR EXTRACTION
	tests, testErr := dig.loadTestResults()
	if testErr == nil && tests != nil {
		failedCount := 0
		for _, t := range tests {
			if t.Result == "failed" {
				failedCount++
			}
		}
		groups, scale := classifyErrors(tests, 10)
		result.Errors = groups
		result.Scale = scale
	} else {
		// 4. FALLBACK: try junit.xml, then report no test results
		if data, err := dig.store.artifact(dig.base, "artifacts/junit_operator.xml"); err == nil {
			result.Scale = FailureScale{HasTestResults: false}
			var fallbackErrors []ErrorGroup
			suites := parseJUnitForTriage(data)
			for _, tc := range suites {
				if tc.err != "" {
					raw := tc.err
					cleaned := stripGoPointers(raw)
					cause, causeTrunc := extractInnermostCause(raw)
					fallbackErrors = append(fallbackErrors, ErrorGroup{
						Signature:              normalizeForSimilarity(raw),
						Error:                  cleaned,
						TestCount:              1,
						Tests:                  []string{tc.name},
						SourceFile:             extractSourceFile(raw),
						InnermostCause:         cause,
						InnermostCauseTruncated: causeTrunc,
					})
				}
			}
			result.Errors = fallbackErrors
			result.Scale.FailedTestCount = len(fallbackErrors)
		} else {
			result.Scale = FailureScale{HasTestResults: false}
		}
	}

	// 5. INFRASTRUCTURE CONTEXT
	if data, err := dig.store.artifact(dig.base, "artifacts/ci-operator-metrics.json"); err == nil {
		result.Metrics = extractMetricsEvents(data)
	}
	if data, err := dig.store.artifact(dig.base, "build-log.txt"); err == nil {
		result.BuildLog = extractBuildLog(data)
	}

	// 6. LINKS
	linkPath := fmt.Sprintf("artifacts/%s/aro-hcp-gather-custom-link-tools/artifacts/custom-link-tools-commands.html", dig.step)
	if data, err := dig.store.artifact(dig.base, linkPath); err == nil {
		result.Links = extractTestLinks(string(data))
	}

	// 7. CORRELATION
	if *contextDays > 0 {
		result.Neighbors = buildNeighborContext(result, *contextDays)
	}

	// 8. PODINFO
	if data, err := dig.store.artifact(dig.base, "podinfo.json"); err == nil {
		result.Podinfo = extractPodinfoSummary(data)
	}

	// 9. EVENTS
	if data, err := dig.store.artifact(dig.base, "artifacts/build-resources/events.json"); err == nil {
		result.Events = extractEventsSummary(data)
	}

	// 10. POOL STATE
	poolPath := fmt.Sprintf("artifacts/%s/%s/artifacts/identities-pool-state.yaml", dig.step, dig.container)
	if data, err := dig.store.artifact(dig.base, poolPath); err == nil {
		result.Pool = extractPoolSummary(data)
	}

	// 11. PROVISION (presubmit only)
	if result.Context.IsPresubmit {
		provPath := fmt.Sprintf("artifacts/%s/aro-hcp-provision-environment/artifacts/junit_entrypoint.xml", dig.step)
		if data, err := dig.store.artifact(dig.base, provPath); err == nil {
			result.Provision = extractProvisionSummary(data)
		}
	}

	// 12. ALERTS (presubmit only)
	if result.Context.IsPresubmit {
		alertPath := fmt.Sprintf("artifacts/%s/aro-hcp-gather-observability/artifacts/alerts.json", dig.step)
		if data, err := dig.store.artifact(dig.base, alertPath); err == nil {
			result.Alerts = extractAlertsSummary(data)
		}
	}

	// 13. AZURE ERRORS (per failing test)
	for _, eg := range result.Errors {
		for _, testName := range eg.Tests {
			azurePath := fmt.Sprintf("artifacts/%s/%s/artifacts/%s/azure.log",
				dig.step, dig.container, sanitizeTest(testName))
			if data, err := dig.store.artifact(dig.base, azurePath); err == nil {
				if summary := extractAzureSummary(data, testName); summary != nil && len(summary.ResponseErrors) > 0 {
					result.Azure = append(result.Azure, *summary)
				}
			}
		}
	}

	return json.NewEncoder(os.Stdout).Encode(result)
}

func buildRunContext(s *RunSummary) RunContext {
	ctx := RunContext{
		IsPresubmit: isPresubmitJob(s.Name),
		PullNumber:  extractPullNumber(s.URL),
	}
	switch {
	case ctx.IsPresubmit:
		ctx.Env = "dev"
	default:
		for env, jobFilter := range map[string]string{
			"int": "integration", "stg": "stage", "prod": "prod",
		} {
			if strings.Contains(s.Name, jobFilter) {
				ctx.Env = env
				break
			}
		}
	}
	return ctx
}

type junitTriageEntry struct {
	name string
	err  string
}

func parseJUnitForTriage(data []byte) []junitTriageEntry {
	suites, err := parseJUnit(data)
	if err != nil {
		return nil
	}
	var results []junitTriageEntry
	for _, suite := range suites.Suites {
		for _, tc := range suite.Cases {
			if f := tc.effectiveFailure(); f != nil {
				results = append(results, junitTriageEntry{
					name: tc.Name,
					err:  f.errorMessage(),
				})
			}
		}
	}
	return results
}

func buildNeighborContext(result *TriageResult, days int) *NeighborContext {
	env := result.Context.Env
	if env == "" || env == "dev" {
		return nil
	}
	release, ok := envRelease[env]
	if !ok {
		return nil
	}
	jobFilter := defaultJobFilter(env)

	s := newSippy()
	runs, err := s.listRuns(release, neighborRunsLimit, jobFilter)
	if err != nil {
		return nil
	}

	cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
	var window []JobRun
	for _, r := range runs {
		if r.Timestamp >= cutoff {
			window = append(window, r)
		}
	}

	nc := &NeighborContext{
		WindowDays: days,
		TotalRuns:  len(window),
	}

	var thisHash string
	for _, r := range window {
		fc := realFailureCount(r)
		if fc == 0 {
			nc.PassedRuns++
		} else {
			nc.FailedRuns++
		}
		if fmt.Sprintf("%d", r.ID) == result.RunID {
			thisHash = ev2Hash(r)
			result.Context.EV2Hash = thisHash
			result.Context.Region = ev2Region(r)
		}
	}

	if thisHash != "" {
		testFails := map[string]int{}
		testPasses := map[string]int{}
		testTotal := map[string]int{}

		for _, r := range window {
			if ev2Hash(r) != thisHash {
				continue
			}
			nc.SameHashRuns++
			fc := realFailureCount(r)
			if fc == 0 {
				nc.SameHashPassed++
			} else {
				nc.SameHashFailed++
			}

			failSet := map[string]bool{}
			for _, name := range r.FailedTestNames {
				if !isSyntheticTest(name) {
					failSet[name] = true
					testFails[name]++
				}
			}
			for _, name := range result.failingTestNames() {
				testTotal[name]++
				if !failSet[name] {
					testPasses[name]++
				}
			}
		}

		for _, name := range result.failingTestNames() {
			nc.TestConsistency = append(nc.TestConsistency, TestFlakeInfo{
				TestName:   name,
				FailedRuns: testFails[name],
				PassedRuns: testPasses[name],
				TotalRuns:  nc.SameHashRuns,
			})
		}
	}

	return nc
}

func (r *TriageResult) failingTestNames() []string {
	var names []string
	for _, eg := range r.Errors {
		names = append(names, eg.Tests...)
	}
	seen := map[string]bool{}
	var unique []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			unique = append(unique, n)
		}
	}
	return unique
}

// outputTailBudget returns how many output tail lines to include per test,
// scaling down as failure count grows to control output size.
