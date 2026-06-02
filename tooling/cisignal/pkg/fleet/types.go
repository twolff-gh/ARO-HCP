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

// types.go defines the internal pipeline type (enrichedRun) and all JSON
// output types for fleet analysis.
package fleet

import "github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"

// enrichedRun is the pipeline's working type for one failed CI run.
// Initialized with Sippy data, then enriched with build-log.txt errors.
type enrichedRun struct {
	Run     sippy.CleanedRun
	Errors  map[string]string
	Signals map[string]*testSignals
}

// Result is the top-level fleet analysis output.
type Result struct {
	Env          string        `json:"env"`
	Days         int           `json:"days"`
	Health       Health        `json:"health"`
	FailingTests []FailingTest `json:"failing_tests,omitempty"`
	RunSummary   *RunSummary   `json:"run_summary,omitempty"`
}

// FailingTest is a test that failed in the analysis window.
// ErrorSamples holds one raw sample per distinct error shape
// (deduped by stripping instance identifiers like UUIDs and
// resource group suffixes — the raw text is preserved).
// PoolRetries/PoolWaitS are worst-case across the window.
type FailingTest struct {
	Test         string        `json:"test"`
	Hits         int           `json:"hits"`
	PreHits      int           `json:"pre_hits"`
	PeriodicHits int           `json:"periodic_hits"`
	PRNumbers    []int         `json:"pr_numbers,omitempty"`
	PassRate     float64       `json:"pass_rate"`
	ErrorSamples []ErrorSample `json:"error_samples"`
	PoolRetries  int           `json:"pool_retries,omitempty"`
	PoolWaitS    float64       `json:"pool_wait_s,omitempty"`
}

// ErrorSample is one distinct error shape with its occurrence count.
// Text is the raw (uncleaned) error from the first matching run.
// URLs are Prow run links for all occurrences of this shape.
type ErrorSample struct {
	Text      string   `json:"text"`
	Count     int      `json:"count"`
	URLs      []string `json:"urls"`
	PRNumbers []int    `json:"pr_numbers,omitempty"`
}

// RunSummary holds aggregate stats for failed runs.
type RunSummary struct {
	TotalRuns    int `json:"total_runs"`
	PRRuns       int `json:"pr_runs"`
	PeriodicRuns int `json:"periodic_runs"`
}

// Health holds fleet-level signals for one environment.
type Health struct {
	PassRate       float64 `json:"pass_rate"`
	TotalRuns      int     `json:"total_runs"`
	PassRate14Day  float64 `json:"pass_rate_14day,omitempty"`
	TotalRuns14Day int     `json:"total_runs_14day,omitempty"`
	Streak         Streak  `json:"streak"`
}

// Streak tracks consecutive passes or failures from the most recent run.
type Streak struct {
	Count int    `json:"count"`
	State string `json:"state"`
	Since string `json:"since,omitempty"`
}

// testHistory holds per-test pass/fail counts over the lookback window.
// Internal only — not exported in JSON output.
type testHistory struct {
	runs   int
	passes int
}
