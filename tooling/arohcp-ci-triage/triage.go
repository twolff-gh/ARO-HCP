package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

type TriageResult struct {
	RunID       string             `json:"run_id"`
	Job         string             `json:"job"`
	Result      string             `json:"result"`
	ProwURL     string             `json:"prow_url"`
	Timestamp   string             `json:"timestamp"`
	DurationSec float64            `json:"duration_seconds"`
	Context     RunContext         `json:"context"`
	TotalTests  int                `json:"total_tests"`
	FailedTests int                `json:"failed_tests"`
	Failures    []TriageFailure    `json:"failures"`
	ErrorGroups []ErrorGroup       `json:"error_groups,omitempty"`
	Steps       []StepTiming       `json:"steps,omitempty"`
	Metrics     *MetricsExtract    `json:"metrics,omitempty"`
	BuildLog    *BuildLogExtract   `json:"build_log,omitempty"`
	Links       []TestLink         `json:"links,omitempty"`
	Neighbors   *NeighborContext   `json:"neighbors,omitempty"`
	Podinfo     *PodinfoSummary    `json:"podinfo,omitempty"`
	Events      *EventsSummary     `json:"events,omitempty"`
	Pool        *PoolSummary       `json:"pool,omitempty"`
	Provision   *ProvisionSummary  `json:"provision,omitempty"`
	Alerts            []AlertSummary     `json:"alerts,omitempty"`
	Azure             []AzureTestSummary `json:"azure,omitempty"`
	LROClassification string             `json:"lro_classification,omitempty"`
}

type RunContext struct {
	Env         string `json:"env"`
	IsPresubmit bool   `json:"is_presubmit"`
	EV2Hash     string `json:"ev2_hash,omitempty"`
	Region      string `json:"region,omitempty"`
	PullNumber  int    `json:"pull_number,omitempty"`
}

type TriageFailure struct {
	Name        string   `json:"name"`
	DurationSec float64  `json:"duration_seconds"`
	Error       string   `json:"error,omitempty"`
	OutputTail  []string `json:"output_tail,omitempty"`
}

type ErrorGroup struct {
	Signature   string   `json:"signature"`
	Count       int      `json:"count"`
	Tests       []string `json:"tests"`
	SampleError string   `json:"sample_error,omitempty"`
}

type NeighborContext struct {
	WindowDays      int             `json:"window_days"`
	TotalRuns       int             `json:"total_runs"`
	PassedRuns      int             `json:"passed_runs"`
	FailedRuns      int             `json:"failed_runs"`
	SameHashRuns    int             `json:"same_hash_runs,omitempty"`
	SameHashPassed  int             `json:"same_hash_passed,omitempty"`
	SameHashFailed  int             `json:"same_hash_failed,omitempty"`
	TestConsistency []TestFlakeInfo `json:"test_consistency,omitempty"`
}

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
		Context:     buildRunContext(s),
	}

	// 1. STEPS
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

	// 2. TESTS
	tests, testErr := dig.loadTestResults()
	if testErr == nil && tests != nil {
		failCount := 0
		for _, t := range tests {
			if t.Result == "failed" {
				failCount++
			}
		}
		for _, t := range tests {
			result.TotalTests++
			if t.Result == "failed" {
				result.FailedTests++
				tf := TriageFailure{
					Name:        t.Name,
					DurationSec: float64(t.Duration) / 1000.0,
					Error:       t.Error,
				}
				tf.OutputTail = extractOutputTail(t.Output, failCount, t.Error)
				result.Failures = append(result.Failures, tf)
			}
		}
		if result.FailedTests > 5 {
			result.ErrorGroups = buildErrorGroups(result.Failures)
		}
	} else if data, err := dig.store.artifact(dig.base, "artifacts/junit_operator.xml"); err == nil {
		entries := parseJUnitForTriage(data)
		for _, tc := range entries {
			result.TotalTests++
			if tc.err != "" {
				result.FailedTests++
				result.Failures = append(result.Failures, TriageFailure{
					Name:  tc.name,
					Error: tc.err,
				})
			}
		}
	}

	// 3. METRICS
	if data, err := dig.store.artifact(dig.base, "artifacts/ci-operator-metrics.json"); err == nil {
		result.Metrics = extractMetricsEvents(data)
	}

	// 4. BUILD LOG
	if data, err := dig.store.artifact(dig.base, "build-log.txt"); err == nil {
		result.BuildLog = extractBuildLog(data)
	}

	// 5. LINKS
	linkPath := fmt.Sprintf("artifacts/%s/aro-hcp-gather-custom-link-tools/artifacts/custom-link-tools-commands.html", dig.step)
	if data, err := dig.store.artifact(dig.base, linkPath); err == nil {
		result.Links = extractTestLinks(string(data))
	}

	// 6. NEIGHBORS
	if *contextDays > 0 {
		result.Neighbors = buildNeighborContext(result, *contextDays)
	}

	// 7. PODINFO
	if data, err := dig.store.artifact(dig.base, "podinfo.json"); err == nil {
		result.Podinfo = extractPodinfoSummary(data)
	}

	// 8. EVENTS
	if data, err := dig.store.artifact(dig.base, "artifacts/build-resources/events.json"); err == nil {
		result.Events = extractEventsSummary(data)
	}

	// 9. POOL
	poolPath := fmt.Sprintf("artifacts/%s/%s/artifacts/identities-pool-state.yaml", dig.step, dig.container)
	if data, err := dig.store.artifact(dig.base, poolPath); err == nil {
		result.Pool = extractPoolSummary(data)
	}

	// 10. PROVISION (presubmit only)
	if result.Context.IsPresubmit {
		provPath := fmt.Sprintf("artifacts/%s/aro-hcp-provision-environment/artifacts/junit_entrypoint.xml", dig.step)
		if data, err := dig.store.artifact(dig.base, provPath); err == nil {
			result.Provision = extractProvisionSummary(data)
		}
	}

	// 11. ALERTS (presubmit only)
	if result.Context.IsPresubmit {
		alertPath := fmt.Sprintf("artifacts/%s/aro-hcp-gather-observability/artifacts/alerts.json", dig.step)
		if data, err := dig.store.artifact(dig.base, alertPath); err == nil {
			result.Alerts = extractAlertsSummary(data)
		}
	}

	// 12. AZURE ERRORS + LRO STATES (per failing test)
	// For cascade runs (>15 failures), sample first 3 to avoid excessive GCS fetches.
	azureLimit := len(result.Failures)
	if azureLimit > 3 && result.FailedTests > 15 {
		azureLimit = 3
	}
	for i, f := range result.Failures {
		if i >= azureLimit {
			break
		}
		azurePath := fmt.Sprintf("artifacts/%s/%s/artifacts/%s/azure.log",
			dig.step, dig.container, sanitizeTest(f.Name))
		if data, err := dig.store.artifact(dig.base, azurePath); err == nil {
			if summary := extractAzureSummary(data, f.Name); summary != nil {
				if len(summary.ResponseErrors) > 0 || len(summary.LROStates) > 0 {
					result.Azure = append(result.Azure, *summary)
				}
			}
		}
	}
	result.LROClassification = classifyLRO(result.Azure)

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
			for _, f := range result.Failures {
				if !failSet[f.Name] {
					testPasses[f.Name]++
				}
			}
		}

		for _, f := range result.Failures {
			nc.TestConsistency = append(nc.TestConsistency, TestFlakeInfo{
				TestName:   f.Name,
				FailedRuns: testFails[f.Name],
				PassedRuns: testPasses[f.Name],
				TotalRuns:  nc.SameHashRuns,
			})
		}
	}

	return nc
}

// extractOutputTail returns the last N lines of a test's stdout output.
// Budget: ≤5 failures → 20 lines per test; 6-20 → 10 lines only when
// error text is diagnostically empty ("Interrupted by User", crash dumps);
// 21+ → skip entirely (cascade dedup is sufficient). These thresholds
// are from SIGNAL-ANALYSIS-UNIFIED.md §2c.
func extractOutputTail(output string, totalFailures int, errorText string) []string {
	if output == "" || totalFailures > 20 {
		return nil
	}
	n := 20
	if totalFailures > 5 {
		if !isDiagnosticallyEmpty(errorText) {
			return nil
		}
		n = 10
	}
	lines := strings.Split(output, "\n")
	var nonEmpty []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			nonEmpty = append(nonEmpty, line)
		}
	}
	if len(nonEmpty) <= n {
		return nonEmpty
	}
	return nonEmpty[len(nonEmpty)-n:]
}

func isDiagnosticallyEmpty(err string) bool {
	return err == "" ||
		strings.Contains(err, "Interrupted by User") ||
		strings.Contains(err, "Deserialization Error")
}

// classifyLRO derives a cluster-timeout sub-classification from sampled
// azure.log LRO state distributions. Classifies per-test and returns
// the dominant classification:
//   - "accepted_stuck": HCP cluster LRO never reached Provisioning (CS layer blocked)
//   - "provisioning_stuck": LRO reached Provisioning but never Succeeded (HyperShift/Maestro)
//   - "": not enough data or healthy pattern
func classifyLRO(azure []AzureTestSummary) string {
	acceptedStuck := 0
	provisioningStuck := 0
	for _, a := range azure {
		acc := a.LROStates["Accepted"]
		prov := a.LROStates["Provisioning"]
		succ := a.LROStates["Succeeded"]
		if acc > 50 && prov == 0 {
			acceptedStuck++
		} else if prov > 50 && succ < 4 {
			provisioningStuck++
		}
	}
	if acceptedStuck > 0 && acceptedStuck >= provisioningStuck {
		return "accepted_stuck"
	}
	if provisioningStuck > 0 {
		return "provisioning_stuck"
	}
	return ""
}

// buildErrorGroups deduplicates failures by normalized error signature.
// Only computed when >5 failures (cascade territory). Groups failures with
// the same normalized error into buckets with count and test list, sorted
// by count descending. Uses normalizeError from normalize.go.
func buildErrorGroups(failures []TriageFailure) []ErrorGroup {
	groups := map[string]*ErrorGroup{}
	var order []string
	for _, f := range failures {
		sig := "(no error text)"
		if f.Error != "" {
			sig = normalizeError(f.Error)
		}
		g, exists := groups[sig]
		if !exists {
			g = &ErrorGroup{Signature: sig, SampleError: f.Error}
			groups[sig] = g
			order = append(order, sig)
		}
		g.Count++
		g.Tests = append(g.Tests, f.Name)
	}
	result := make([]ErrorGroup, 0, len(order))
	for _, sig := range order {
		result = append(result, *groups[sig])
	}
	slices.SortFunc(result, func(a, b ErrorGroup) int {
		return cmp.Compare(b.Count, a.Count)
	})
	return result
}
