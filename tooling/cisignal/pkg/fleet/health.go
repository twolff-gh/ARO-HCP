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

// health.go computes fleet health signals: pass rates, streaks,
// and failed run selection.
package fleet

import "github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"

// computeHealth derives pass rate from window runs and streak from
// streakRuns (typically EV2-only, to track deployment outcomes
// without nightly/PR noise). When days < historyLookbackDays,
// allRuns provides 14-day trend context.
func computeHealth(runs, streakRuns, allRuns []sippy.CleanedRun, days int) Health {
	if len(runs) == 0 {
		return Health{}
	}
	passed := 0
	for _, r := range runs {
		if r.Passed {
			passed++
		}
	}
	h := Health{
		PassRate:  float64(passed) / float64(len(runs)) * 100,
		TotalRuns: len(runs),
		Streak:    computeStreak(streakRuns),
	}
	if days < historyLookbackDays && len(allRuns) > len(runs) {
		passed14 := 0
		for _, r := range allRuns {
			if r.Passed {
				passed14++
			}
		}
		h.PassRate14Day = float64(passed14) / float64(len(allRuns)) * 100
		h.TotalRuns14Day = len(allRuns)
	}
	return h
}

// computeStreak counts consecutive passes or failures from the newest run.
// Runs must be sorted newest-first. Sets Since to the timestamp of the
// oldest run in the streak so the reader knows when the streak started.
func computeStreak(runs []sippy.CleanedRun) Streak {
	if len(runs) == 0 {
		return Streak{}
	}
	first := runs[0].Passed
	count := 0
	var oldestTS int64
	for _, r := range runs {
		if r.Passed != first {
			break
		}
		count++
		oldestTS = r.Timestamp
	}
	state := "red"
	if first {
		state = "green"
	}
	s := Streak{Count: count, State: state}
	if oldestTS > 0 {
		s.Since = sippy.FormatTimestamp(oldestTS)
	}
	return s
}

// selectFailed picks runs with real test failures (not infra-only).
func selectFailed(runs []sippy.CleanedRun) []sippy.CleanedRun {
	var selected []sippy.CleanedRun
	for _, r := range runs {
		if !r.Passed && len(r.RealFailures) > 0 {
			selected = append(selected, r)
		}
	}
	return selected
}

// filterPeriodic keeps only periodic runs (PullNumber == 0),
// excluding PR-triggered presubmit runs.
func filterPeriodic(runs []sippy.CleanedRun) []sippy.CleanedRun {
	var out []sippy.CleanedRun
	for _, r := range runs {
		if r.PullNumber == 0 {
			out = append(out, r)
		}
	}
	return out
}
