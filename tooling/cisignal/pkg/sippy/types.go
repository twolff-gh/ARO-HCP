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

// types.go defines Sippy API response types (rawRun, ev2Annotations) and the
// cleaned run types (CleanedRun, PartitionedRuns) used by fleet analysis.
package sippy

import "time"

const resultSucceeded = "S"

// ev2Annotations holds the EV2 deployment metadata from Sippy run annotations.
type ev2Annotations struct {
	Hash   string `json:"ev2.rollout/ARO-HCP"`
	Region string `json:"ev2.rollout/region"`
}

// rawRun is the direct JSON mapping from Sippy /api/jobs/runs.
type rawRun struct {
	ID              int64           `json:"id"`
	Job             string          `json:"job"`
	OverallResult   string          `json:"overall_result"`
	FailedTestNames []string        `json:"failed_test_names"`
	Timestamp       int64           `json:"timestamp"`
	Annotations     *ev2Annotations `json:"annotations"`
	URL      string `json:"url"`
	PRAuthor string `json:"pull_request_author"`
}

// CleanedRun is a post-cleanup run ready for computation.
type CleanedRun struct {
	ID           int64    `json:"id"`
	Job          string   `json:"job"`
	Timestamp    int64    `json:"timestamp"`
	Passed       bool     `json:"passed"`
	RealFailures []string `json:"real_failures"`
	EV2Hash      string   `json:"ev2_hash,omitempty"`
	URL          string   `json:"url"`
	PullNumber   int      `json:"pull_number,omitempty"`
}

// PartitionedRuns separates EV2 deployment runs from nightly/cron runs.
type PartitionedRuns struct {
	EV2     []CleanedRun `json:"ev2"`
	Nightly []CleanedRun `json:"nightly"`
	All     []CleanedRun `json:"all"`
}

// FormatTimestamp converts a Unix millisecond timestamp to RFC3339 UTC.
func FormatTimestamp(millis int64) string {
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}

// FilterByDays returns runs whose timestamp falls within the last N days.
func FilterByDays(runs []CleanedRun, days int) []CleanedRun {
	cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
	var out []CleanedRun
	for _, r := range runs {
		if r.Timestamp >= cutoff {
			out = append(out, r)
		}
	}
	return out
}
