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

// fetch.go resolves environment names to Sippy parameters, fetches and cleans
// runs, partitions by EV2 deployment hash, and sorts newest-first.
package sippy

import (
	"cmp"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	presubmitRelease = "Presubmits"
	syntheticPrefix  = "[sig-sippy]"
	prowMetaTest     = "Job run should complete before timeout"
)

// PartitionByEV2 separates runs triggered by EV2 deployments from scheduled
// nightly/cron runs. EV2 runs carry a deployment hash and are used for onset
// detection and deployment correlation. Nightly runs lack deployment context
// and are aggregated separately to avoid skewing pass rates.
func PartitionByEV2(runs []CleanedRun) PartitionedRuns {
	p := PartitionedRuns{All: runs}
	for _, r := range runs {
		if r.EV2Hash != "" {
			p.EV2 = append(p.EV2, r)
		} else {
			p.Nightly = append(p.Nightly, r)
		}
	}
	return p
}

// needsPresubmitFetch returns true when the environment's primary Sippy
// release is not already Presubmits, meaning presubmit runs (pull-ci-*)
// are in a separate release and require a second query.
func needsPresubmitFetch(env string) bool {
	cfg, ok := envConfigs[env]
	return ok && cfg.release != presubmitRelease
}

// mergeRuns appends runs from extra into base, skipping duplicates by ID.
func mergeRuns(base, extra []rawRun) []rawRun {
	seen := make(map[int64]struct{}, len(base))
	for _, r := range base {
		seen[r.ID] = struct{}{}
	}
	for _, r := range extra {
		if _, dup := seen[r.ID]; !dup {
			base = append(base, r)
		}
	}
	return base
}

// FetchCleanedRuns resolves an environment name to Sippy parameters, fetches
// runs for the given lookback window, cleans them (filters synthetics, extracts
// EV2 fields), and returns them sorted newest-first.
//
// For non-dev environments, a second query fetches presubmit (pull-ci-*) runs
// from the Presubmits release, since those jobs match the same job filter but
// are indexed under a different Sippy release than postsubmit/periodic runs.
func FetchCleanedRuns(env string, days int) ([]CleanedRun, error) {
	rel, filter, err := EnvConfig(env)
	if err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -days)
	client := NewClient()
	raw, err := client.ListRuns(rel, filter, since)
	if err != nil {
		return nil, err
	}

	if needsPresubmitFetch(env) {
		prRaw, prErr := client.ListRuns(presubmitRelease, filter, since)
		if prErr != nil {
			slog.Warn("presubmit fetch failed, continuing without PR runs",
				"env", env, "error", prErr)
		} else {
			slog.Info("presubmit runs fetched", "env", env, "count", len(prRaw))
			raw = mergeRuns(raw, prRaw)
		}
	}

	cleaned := cleanRuns(raw)
	slices.SortFunc(cleaned, CompareTimestampDesc)
	return cleaned, nil
}

// cleanRuns converts a batch of raw Sippy runs into CleanedRuns.
func cleanRuns(raw []rawRun) []CleanedRun {
	out := make([]CleanedRun, 0, len(raw))
	for _, r := range raw {
		out = append(out, cleanRun(r))
	}
	return out
}

// cleanRun maps one raw Sippy run to a CleanedRun, filtering synthetic test
// names and extracting EV2 deployment fields from annotations.
func cleanRun(r rawRun) CleanedRun {
	var real []string
	for _, name := range r.FailedTestNames {
		if !isSynthetic(name) {
			real = append(real, name)
		}
	}

	var hash string
	if r.Annotations != nil {
		hash = r.Annotations.Hash
	}

	return CleanedRun{
		ID:           r.ID,
		Job:          r.Job,
		Timestamp:    r.Timestamp,
		Passed:       r.OverallResult == resultSucceeded,
		RealFailures: real,
		EV2Hash:      hash,
		URL:          r.URL,
		PullNumber:   ExtractPullNumber(r.URL),
	}
}

func isSynthetic(name string) bool {
	return strings.HasPrefix(name, syntheticPrefix) || name == prowMetaTest
}

// ExtractPullNumber extracts the PR number from a Prow job URL.
// Returns 0 for non-presubmit URLs.
func ExtractPullNumber(prowURL string) int {
	_, rest, found := strings.Cut(prowURL, "/pull/")
	if !found {
		return 0
	}
	_, rest, found = strings.Cut(rest, "/")
	if !found {
		return 0
	}
	end := strings.Index(rest, "/")
	if end < 0 {
		end = len(rest)
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}

// CompareTimestampDesc sorts CleanedRuns newest-first. For use with slices.SortFunc.
func CompareTimestampDesc(a, b CleanedRun) int {
	return cmp.Compare(b.Timestamp, a.Timestamp)
}
