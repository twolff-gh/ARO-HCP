// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Pipeline phases:
//  1. Fetch — pull runs from Sippy for [days] + [historyLookbackDays]
//  2. Partition — separate EV2 deployment runs from nightly/cron
//  3. Health — compute pass rate, streak
//  4. Enrich — fetch build-log.txt from GCS
//  5. History — compute per-test pass/fail over the history window
//  6. Failing tests — group by test name, collect errors + pool signals
package fleet

import (
	"cmp"
	"log/slog"
	"slices"
	"sort"

	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/gcs"
	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"
)

const (
	historyLookbackDays = 14   // 14d trend context for pass rates
	maxOutputErrorLen   = 1000 // cap per-test error text to keep triage output scannable
)

// Analyze executes the fleet analysis pipeline for one environment.
// When periodic is true, only periodic runs (not PR-triggered) are
// enriched and included in failing tests.
func Analyze(env string, days int, periodic bool) (*Result, error) {
	// Phase 1: Fetch runs from Sippy.
	fetchDays := max(days, historyLookbackDays)
	allRuns, err := sippy.FetchCleanedRuns(env, fetchDays)
	if err != nil {
		return nil, err
	}
	runs := sippy.FilterByDays(allRuns, days)

	// Phase 2: Partition by EV2 deployment hash.
	p := sippy.PartitionByEV2(runs)

	// Phase 3: Compute fleet health from all runs.
	streakRuns := p.EV2
	if len(streakRuns) == 0 {
		streakRuns = runs
	}
	health := computeHealth(runs, streakRuns, allRuns, days)

	slog.Info("health", "env", env,
		"pass_rate", health.PassRate,
		"total", health.TotalRuns,
		"streak", health.Streak.Count, "state", health.Streak.State)

	// Phase 4: Enrich failed runs with build-log.txt errors.
	g := gcs.NewClient()
	failed := selectFailed(runs)
	if periodic {
		failed = filterPeriodic(failed)
	}
	enriched := enrichRuns(failed, env, g)

	// Phase 5: Compute per-test history over the full lookback window.
	history := computeHistory(enriched, allRuns)

	// Phase 6: Build failing test list.
	failingTests := buildFailingTests(enriched, history)

	return &Result{
		Env:          env,
		Days:         days,
		Health:       health,
		FailingTests: failingTests,
		RunSummary:   computeRunSummary(enriched),
	}, nil
}

// buildFailingTests collects errors per test, deduplicating by normalized
// key (instance identifiers stripped). Each distinct error shape keeps one
// raw sample and a count. The LLM sees every distinct shape; identical
// repetitions are collapsed.
func buildFailingTests(enriched []enrichedRun, history map[string]testHistory) []FailingTest {
	type errorShape struct {
		text      string
		count     int
		urls      []string
		prNumbers map[int]bool
	}
	type testInfo struct {
		shapes       map[string]*errorShape // dedupKey -> shape
		shapeOrder   []string               // insertion order
		hits         int
		preHits      int
		periodicHits int
		prNumbers    map[int]bool
		maxRetries   int
		maxWaitS     float64
	}
	tests := map[string]*testInfo{}

	for _, r := range enriched {
		for name, errText := range r.Errors {
			trimmed := truncate(errText, maxOutputErrorLen)
			info, ok := tests[name]
			if !ok {
				info = &testInfo{
					shapes:    map[string]*errorShape{},
					prNumbers: map[int]bool{},
				}
				tests[name] = info
			}
			info.hits++
			if r.Run.PullNumber > 0 {
				info.preHits++
				info.prNumbers[r.Run.PullNumber] = true
			} else {
				info.periodicHits++
			}
			key := dedupKey(trimmed)
			if s, exists := info.shapes[key]; exists {
				s.count++
				s.urls = append(s.urls, r.Run.URL)
				if r.Run.PullNumber > 0 {
					s.prNumbers[r.Run.PullNumber] = true
				}
			} else {
				prs := map[int]bool{}
				if r.Run.PullNumber > 0 {
					prs[r.Run.PullNumber] = true
				}
				info.shapes[key] = &errorShape{
					text:      trimmed,
					count:     1,
					urls:      []string{r.Run.URL},
					prNumbers: prs,
				}
				info.shapeOrder = append(info.shapeOrder, key)
			}
			if s := r.Signals[name]; s != nil {
				if s.PoolRetries > info.maxRetries {
					info.maxRetries = s.PoolRetries
				}
				if s.PoolWaitS > info.maxWaitS {
					info.maxWaitS = s.PoolWaitS
				}
			}
		}
	}

	var result []FailingTest
	for name, info := range tests {
		samples := make([]ErrorSample, 0, len(info.shapeOrder))
		for _, key := range info.shapeOrder {
			s := info.shapes[key]
			samples = append(samples, ErrorSample{
				Text:      s.text,
				Count:     s.count,
				URLs:      s.urls,
				PRNumbers: sortedKeys(s.prNumbers),
			})
		}
		ft := FailingTest{
			Test:         name,
			Hits:         info.hits,
			PreHits:      info.preHits,
			PeriodicHits: info.periodicHits,
			PRNumbers:    sortedKeys(info.prNumbers),
			ErrorSamples: samples,
		}
		if h, ok := history[name]; ok {
			if h.runs > 0 {
				ft.PassRate = float64(h.passes) / float64(h.runs) * 100
			}
		}
		ft.PoolRetries = info.maxRetries
		ft.PoolWaitS = info.maxWaitS
		result = append(result, ft)
	}

	slices.SortFunc(result, func(a, b FailingTest) int {
		if c := cmp.Compare(a.PassRate, b.PassRate); c != 0 {
			return c
		}
		return cmp.Compare(a.Test, b.Test)
	})

	return result
}

func sortedKeys(m map[int]bool) []int {
	if len(m) == 0 {
		return nil
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// computeRunSummary computes aggregate stats from enriched runs.
func computeRunSummary(enriched []enrichedRun) *RunSummary {
	s := &RunSummary{TotalRuns: len(enriched)}
	for _, r := range enriched {
		if r.Run.PullNumber > 0 {
			s.PRRuns++
		} else {
			s.PeriodicRuns++
		}
	}
	return s
}

// computeHistory computes per-test pass/fail counts for every test
// that failed in the enriched runs, using the full history window.
//
// Limitation: Sippy only reports which tests failed, not which passed.
// A test absent from a run's failure list is counted as "not failed" —
// this includes both actual passes and tests that were skipped or not
// present in the suite. The pass count is therefore an upper bound.
func computeHistory(enriched []enrichedRun, historyRuns []sippy.CleanedRun) map[string]testHistory {
	failedTests := map[string]bool{}
	for _, r := range enriched {
		for name := range r.Errors {
			failedTests[name] = true
		}
	}
	if len(failedTests) == 0 {
		return nil
	}

	counts := map[string]*testHistory{}
	for _, run := range historyRuns {
		runFailed := map[string]bool{}
		for _, name := range run.RealFailures {
			runFailed[name] = true
		}

		for name := range failedTests {
			h := counts[name]
			if h == nil {
				h = &testHistory{}
				counts[name] = h
			}
			h.runs++
			if !runFailed[name] {
				h.passes++
			}
		}
	}

	result := make(map[string]testHistory, len(counts))
	for name, h := range counts {
		result[name] = *h
	}
	return result
}
