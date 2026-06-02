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

// enrich.go fetches full error text for failed runs from Sippy summaries
// and GCS build-log.txt, replacing Sippy's 256-char truncations with the
// complete error from the test harness output.
package fleet

import (
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/gcs"
	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"
)

const fetchConcurrency = 10 // parallel GCS fetches; higher risks rate limiting

// enrichRuns fetches full error text for each failed run from Sippy
// and GCS, then replaces truncated errors with build-log.txt content.
func enrichRuns(failed []sippy.CleanedRun, env string, g gcs.Fetcher) []enrichedRun {
	runs := fetchSummaries(failed)
	enrichFromGCS(runs, env, g)
	return runs
}

// fetchSummaries fetches per-run failure details from Sippy in parallel.
func fetchSummaries(failed []sippy.CleanedRun) []enrichedRun {
	runs := make([]enrichedRun, len(failed))
	sc := sippy.NewClient()

	var fetched, skipped atomic.Int64
	parallel(len(failed), func(i int) {
		runs[i].Run = failed[i]
		summary, err := sc.FetchRunSummary(strconv.FormatInt(failed[i].ID, 10))
		if err != nil {
			slog.Warn("sippy error", "run", failed[i].ID, "error", err)
			skipped.Add(1)
			return
		}
		runs[i].Errors = summary.TestFailures
		fetched.Add(1)
	})

	slog.Info("sippy failures", "fetched", fetched.Load(), "skipped", skipped.Load(), "total", len(failed))
	return runs
}

// enrichFromGCS replaces Sippy's truncated errors with full text from
// GCS build-log.txt.
func enrichFromGCS(runs []enrichedRun, env string, g gcs.Fetcher) {
	if len(runs) == 0 {
		return
	}

	var enriched, skipped atomic.Int64
	parallel(len(runs), func(i int) {
		if runs[i].Errors == nil {
			return
		}
		if enrichOneRun(&runs[i], g, env) {
			enriched.Add(1)
		} else {
			skipped.Add(1)
		}
	})

	slog.Info("gcs enrichment", "enriched", enriched.Load(), "skipped", skipped.Load(), "total", len(runs))
}

// enrichOneRun fetches GCS artifacts for one run:
// extension_test_result (or build-log.txt fallback) for full error text.
func enrichOneRun(r *enrichedRun, g gcs.Fetcher, env string) bool {
	paths, err := gcs.NewRunPaths(r.Run.URL, env, g)
	if err != nil {
		slog.Debug("gcs path resolution failed", "run", r.Run.ID, "error", err)
		return false
	}

	signals := fetchTestSignals(g, paths)
	if signals == nil {
		return false
	}
	r.Signals = signals
	replaced := false
	for name, sippyErr := range r.Errors {
		if sig, ok := signals[name]; ok && len(sig.Error) > len(sippyErr) {
			r.Errors[name] = sig.Error
			replaced = true
		}
	}
	return replaced
}

// fetchTestSignals tries extension_test_result first (clean JSON),
// then falls back to build-log.txt (embedded JSON).
func fetchTestSignals(g gcs.Fetcher, paths gcs.RunPaths) map[string]*testSignals {
	if data, err := g.FindByPrefix(paths.TestResultsPrefix(), ".json"); err == nil {
		if signals := parseTestResults(data); signals != nil {
			return signals
		}
	}
	if data, err := g.Artifact(paths.BuildLog()); err == nil {
		return parseBuildLog(data)
	}
	return nil
}

// parallel runs fn(0), fn(1), ..., fn(n-1) concurrently with bounded parallelism.
func parallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}
