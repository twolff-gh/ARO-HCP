package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type watchJSON struct {
	Fleet        watchFleetJSON        `json:"fleet"`
	PRAdvisories []watchPRAdvisoryJSON `json:"pr_advisories,omitempty"`
}

type watchFleetJSON struct {
	PassRate     float64  `json:"pass_rate"`
	TotalRuns    int      `json:"total_runs"`
	Baseline     float64  `json:"baseline_pass_rate"`
	BaselineRuns int      `json:"baseline_runs"`
	TodayRate    float64  `json:"today_pass_rate"`
	TodayRuns    int      `json:"today_runs"`
	TopSignature string   `json:"top_signature,omitempty"`
}

type watchPRAdvisoryJSON struct {
	PR           int    `json:"pr"`
	TotalRuns    int    `json:"total_runs"`
	PassedRuns   int    `json:"passed_runs"`
	InfraFails   int    `json:"infra_fails"`
	BuildFails   int    `json:"build_fails"`
	TestFails    int    `json:"test_fails"`
	Verdict      string `json:"verdict"`
	TopSignature string `json:"top_signature,omitempty"`
}

type prAcc struct {
	runs         map[int64]bool
	passed       int
	infra        int
	build        int
	test         int
	topSigHits   map[string]int
	latestTS     int64
	latestPassed bool
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	days := fs.Int("days", 5, "lookback days for presubmit")
	prNum := fs.Int("pr", 0, "single PR number to check")
	fs.Parse(args)

	s := newSippy()

	if *prNum != 0 {
		return runWatchPR(s, *prNum, *days)
	}

	data, err := fetchSurveyData(s, "dev", *days, "", "")
	if err != nil {
		return fmt.Errorf("fetching survey data: %w", err)
	}
	sj := buildSurveyJSON("dev", data)

	fleet := watchFleetJSON{
		PassRate:  sj.Status.PassRate,
		TotalRuns: sj.Status.TotalRuns,
	}

	if sj.Status.Baseline != nil {
		fleet.Baseline = sj.Status.Baseline.PassRate
		fleet.BaselineRuns = sj.Status.Baseline.TotalRuns
	}

	if len(sj.DailyRates) > 0 {
		today := sj.DailyRates[len(sj.DailyRates)-1]
		if today.Total > 0 {
			fleet.TodayRate = 100.0 * float64(today.Pass) / float64(today.Total)
			fleet.TodayRuns = today.Total
		}
	}

	if len(sj.Signatures) > 0 {
		fleet.TopSignature = sj.Signatures[0].Key
	}

	// Count all runs per PR (including passes) from raw Sippy data
	prData := map[int]*prAcc{}
	for _, r := range data.runs {
		pr := extractPullNumber(r.URL)
		if pr == 0 {
			continue
		}
		acc, ok := prData[pr]
		if !ok {
			acc = &prAcc{runs: map[int64]bool{}, topSigHits: map[string]int{}}
			prData[pr] = acc
		}
		acc.runs[r.ID] = true
		passed := realFailureCount(r) == 0
		if passed {
			acc.passed++
		}
		if r.Timestamp > acc.latestTS {
			acc.latestTS = r.Timestamp
			acc.latestPassed = passed
		}
	}

	// PR failure classification from signatures
	testToSig := map[string]string{}
	for _, sig := range sj.Signatures {
		for _, t := range sig.Tests {
			testToSig[t] = sig.Key
		}
	}

	runToPR := map[int64]int{}
	for _, r := range sj.Runs {
		if r.PullNumber != 0 {
			runToPR[r.ID] = r.PullNumber
		}
	}

	for _, f := range sj.Failures {
		sigKey := testToSig[f.TestName]
		cls := classifySignatureKey(sigKey)

		for _, o := range f.Outputs {
			pr := runToPR[o.RunID]
			if pr == 0 {
				continue
			}
			acc := prData[pr]
			if acc == nil {
				continue
			}
			acc.topSigHits[sigKey]++

			switch cls {
			case "infra":
				acc.infra++
			case "build":
				acc.build++
			default:
				acc.test++
			}
		}
	}

	// Sort by run count descending
	type prSort struct {
		pr  int
		acc *prAcc
	}
	var sorted []prSort
	for pr, acc := range prData {
		sorted = append(sorted, prSort{pr, acc})
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if len(sorted[j].acc.runs) > len(sorted[i].acc.runs) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var advisories []watchPRAdvisoryJSON
	for _, ps := range sorted {
		topSig := ""
		topCount := 0
		for sig, count := range ps.acc.topSigHits {
			if count > topCount {
				topSig = sig
				topCount = count
			}
		}

		advisories = append(advisories, watchPRAdvisoryJSON{
			PR:           ps.pr,
			TotalRuns:    len(ps.acc.runs),
			PassedRuns:   ps.acc.passed,
			InfraFails:   ps.acc.infra,
			BuildFails:   ps.acc.build,
			TestFails:    ps.acc.test,
			Verdict:      classifyPRVerdict(ps.acc),
			TopSignature: topSig,
		})
	}

	return json.NewEncoder(os.Stdout).Encode(watchJSON{
		Fleet:        fleet,
		PRAdvisories: advisories,
	})
}

func runWatchPR(s *sippy, prNum int, days int) error {
	release := envRelease["dev"]
	runs, err := s.listRuns(release, maxRunsFetch, "e2e-parallel")
	if err != nil {
		return fmt.Errorf("fetching runs: %w", err)
	}

	runs, _ = filterNightlyRuns(runs)
	windowCutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	var baselineRuns []JobRun
	var windowRuns []JobRun
	for _, r := range runs {
		if time.UnixMilli(r.Timestamp).Before(windowCutoff) {
			baselineRuns = append(baselineRuns, r)
		} else {
			windowRuns = append(windowRuns, r)
		}
	}

	// Fleet pass rate
	passed := 0
	for _, r := range windowRuns {
		if realFailureCount(r) == 0 {
			passed++
		}
	}
	fleetRate := 0.0
	if len(windowRuns) > 0 {
		fleetRate = 100.0 * float64(passed) / float64(len(windowRuns))
	}

	baselinePassed := 0
	for _, r := range baselineRuns {
		if realFailureCount(r) == 0 {
			baselinePassed++
		}
	}
	baselineRate := 0.0
	if len(baselineRuns) > 0 {
		baselineRate = 100.0 * float64(baselinePassed) / float64(len(baselineRuns))
	}

	// Today's rate
	todayStr := time.Now().UTC().Format("2006-01-02")
	todayPassed, todayTotal := 0, 0
	for _, r := range windowRuns {
		if time.UnixMilli(r.Timestamp).UTC().Format("2006-01-02") == todayStr {
			todayTotal++
			if realFailureCount(r) == 0 {
				todayPassed++
			}
		}
	}
	todayRate := 0.0
	if todayTotal > 0 {
		todayRate = 100.0 * float64(todayPassed) / float64(todayTotal)
	}

	// Find this PR's runs
	var prRuns []JobRun
	for _, r := range windowRuns {
		if extractPullNumber(r.URL) == prNum {
			prRuns = append(prRuns, r)
		}
	}

	fleet := watchFleetJSON{
		PassRate:     fleetRate,
		TotalRuns:    len(windowRuns),
		Baseline:     baselineRate,
		BaselineRuns: len(baselineRuns),
		TodayRate:    todayRate,
		TodayRuns:    todayTotal,
	}

	if len(prRuns) == 0 {
		result := map[string]any{
			"pr":    prNum,
			"found": false,
			"fleet": fleet,
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}

	// Classify this PR's failures
	acc := &prAcc{runs: map[int64]bool{}, topSigHits: map[string]int{}}
	for _, r := range prRuns {
		passed := realFailureCount(r) == 0
		if r.Timestamp > acc.latestTS {
			acc.latestTS = r.Timestamp
			acc.latestPassed = passed
		}
		if passed {
			acc.passed++
			continue
		}
		acc.runs[r.ID] = true
		for _, name := range r.FailedTestNames {
			if isSyntheticTest(name) {
				continue
			}
			cls := classifySignatureKey(name)
			acc.topSigHits[name]++
			switch cls {
			case "infra":
				acc.infra++
			case "build":
				acc.build++
			default:
				acc.test++
			}
		}
	}

	topSig := ""
	topCount := 0
	for sig, count := range acc.topSigHits {
		if count > topCount {
			topSig = sig
			topCount = count
		}
	}

	result := map[string]any{
		"pr":    prNum,
		"found": true,
		"fleet": fleet,
		"advisory": watchPRAdvisoryJSON{
			PR:           prNum,
			TotalRuns:    len(prRuns),
			PassedRuns:   acc.passed,
			InfraFails:   acc.infra,
			BuildFails:   acc.build,
			TestFails:    acc.test,
			Verdict:      classifyPRVerdict(acc),
			TopSignature: topSig,
		},
	}

	return json.NewEncoder(os.Stdout).Encode(result)
}

func classifySignatureKey(key string) string {
	infraPatterns := []string{
		"ARM step", "poll deployment", "create deployment",
		"Grafana Datasources", "Image Mirror", "Shell Step",
		"timeout", "Timed out", "deadline exceeded",
		"pre steps failed", "test steps failed", "Interrupted",
		"cleanup resource", "Firing", "kubeconfig",
		"failed to create cluster", "failed to create NodePool",
		"failed waiting for nodepool", "failed starting cluster",
		"failed waiting for cluster", "complete before timeout",
		"Run pipeline step", "pipeline step",
	}
	buildPatterns := []string{
		"build src", "build aro-hcp", "could not get build",
		"Build image",
	}

	for _, p := range buildPatterns {
		if strings.Contains(key, p) {
			return "build"
		}
	}
	for _, p := range infraPatterns {
		if strings.Contains(key, p) {
			return "infra"
		}
	}
	return "test"
}

func classifyPRVerdict(acc *prAcc) string {
	totalFails := acc.infra + acc.build + acc.test
	if totalFails == 0 {
		return "passing"
	}
	if acc.latestPassed {
		return "latest_passed"
	}
	if acc.build > 0 && acc.infra == 0 && acc.test == 0 {
		return "code"
	}
	if acc.infra > 0 && acc.build == 0 {
		return "infra"
	}
	if acc.infra > acc.build*2 {
		return "mostly_infra"
	}
	if acc.build > 0 {
		return "mixed"
	}
	return "test_failures"
}
