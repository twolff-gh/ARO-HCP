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

// Package ops checks the health of non-e2e CI jobs (cleanup, pipeline,
// sweeper) by reading pass/fail status directly from GCS artifacts.
// These jobs are not indexed by Sippy.
package ops

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/gcs"
)

const (
	prowBaseURL   = "https://prow.ci.openshift.org/view/gs/test-platform-results/"
	gcsLogsPrefix = "logs/"
	fetchWorkers  = 6  // parallel job fetches; one GCS listing + N artifact fetches per job
	maxRunsPerJob = 30 // recent runs to check per job; covers ~10 days at 3 runs/day
)

type jobDef struct {
	Name  string
	Short string
	Env   string
}

var jobs = []jobDef{
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-integration-resource-groups", Short: "cleanup-int-rg", Env: "int"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-stage-resource-groups", Short: "cleanup-stg-rg", Env: "stg"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-prod-resource-groups", Short: "cleanup-prod-rg", Env: "prod"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-dev-pers-resource-groups", Short: "cleanup-dev-pers-rg", Env: "dev"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-dev-prow-resource-groups", Short: "cleanup-dev-prow-rg", Env: "dev"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-sweeper-rg-ordered", Short: "sweeper-rg", Env: "all"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-sweeper-shared-leftovers-dev", Short: "sweeper-dev", Env: "dev"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-kusto-role-assignments", Short: "cleanup-kusto-roles", Env: "all"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-msft-test-tenant-app-registrations", Short: "cleanup-msft-apps", Env: "all"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-cleanup-delete-expired-red-hat-tenant-app-registrations", Short: "cleanup-rh-apps", Env: "all"},
	{Name: "branch-ci-Azure-ARO-HCP-main-cspr-pipeline-postsubmit", Short: "cspr-pipeline", Env: "dev"},
	{Name: "branch-ci-Azure-ARO-HCP-main-global-pipeline-postsubmit", Short: "global-pipeline", Env: "all"},
	{Name: "periodic-ci-Azure-ARO-HCP-main-image-updater-image-updater-tooling", Short: "image-updater", Env: "all"},
}

type Result struct {
	Days int       `json:"days"`
	Jobs []JobInfo `json:"jobs"`
}

type JobInfo struct {
	Name      string    `json:"name"`
	Short     string    `json:"short"`
	Env       string    `json:"env"`
	Runs      int       `json:"runs"`
	Passes    int       `json:"passes"`
	Failures  int       `json:"failures"`
	Streak    Streak    `json:"streak"`
	LastPass  string    `json:"last_pass,omitempty"`
	LatestRun *RunInfo  `json:"latest_run,omitempty"`
}

type Streak struct {
	Count int    `json:"count"`
	State string `json:"state"`
}

type RunInfo struct {
	ID        string  `json:"id"`
	URL       string  `json:"url"`
	Timestamp string  `json:"timestamp"`
	Result    string  `json:"result"`
	DurationS float64 `json:"duration_s"`
	Error     string  `json:"error,omitempty"`
}

type gcsStarted struct {
	Timestamp int64 `json:"timestamp"`
}

type gcsFinished struct {
	Timestamp int64  `json:"timestamp"`
	Result    string `json:"result"`
	Passed    bool   `json:"passed"`
}

// Analyze checks the health of all ops jobs within the given time window.
func Analyze(days int, g gcs.Fetcher) *Result {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	result := &Result{Days: days}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchWorkers)

	for _, jd := range jobs {
		wg.Add(1)
		go func(jd jobDef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info := analyzeJob(jd, cutoff, g)
			mu.Lock()
			result.Jobs = append(result.Jobs, info)
			mu.Unlock()
		}(jd)
	}
	wg.Wait()

	sort.Slice(result.Jobs, func(i, j int) bool {
		if result.Jobs[i].Failures != result.Jobs[j].Failures {
			return result.Jobs[i].Failures > result.Jobs[j].Failures
		}
		return result.Jobs[i].Short < result.Jobs[j].Short
	})

	return result
}

func analyzeJob(jd jobDef, cutoff time.Time, g gcs.Fetcher) JobInfo {
	info := JobInfo{
		Name:  jd.Name,
		Short: jd.Short,
		Env:   jd.Env,
	}

	prefix := gcsLogsPrefix + jd.Name + "/"
	dirs, _, err := g.ListDir(prefix)
	if err != nil {
		slog.Debug("ops: list failed", "job", jd.Short, "error", err)
		return info
	}

	// Take last N runs (dirs are sorted lexicographically = by run ID = chronological)
	if len(dirs) > maxRunsPerJob {
		dirs = dirs[len(dirs)-maxRunsPerJob:]
	}

	type runResult struct {
		id       string
		started  time.Time
		finished time.Time
		result   string
	}

	var runs []runResult
	for _, dir := range dirs {
		id := strings.TrimPrefix(dir, prefix)
		id = strings.TrimSuffix(id, "/")
		if id == "" || id == "latest-build.txt" {
			continue
		}

		startData, err := g.Artifact(dir + "started.json")
		if err != nil {
			continue
		}
		var s gcsStarted
		if err := json.Unmarshal(startData, &s); err != nil {
			continue
		}
		started := time.Unix(s.Timestamp, 0)
		if started.Before(cutoff) {
			continue
		}

		finData, err := g.Artifact(dir + "finished.json")
		if err != nil {
			// Still running
			continue
		}
		var f gcsFinished
		if err := json.Unmarshal(finData, &f); err != nil {
			continue
		}

		runs = append(runs, runResult{
			id:       id,
			started:  started,
			finished: time.Unix(f.Timestamp, 0),
			result:   f.Result,
		})
	}

	info.Runs = len(runs)
	var lastPass time.Time
	for _, r := range runs {
		switch r.result {
		case "SUCCESS":
			info.Passes++
			if r.finished.After(lastPass) {
				lastPass = r.finished
			}
		default:
			info.Failures++
		}
	}
	if !lastPass.IsZero() {
		info.LastPass = lastPass.UTC().Format(time.RFC3339)
	}

	// Streak: count from newest
	if len(runs) > 0 {
		newest := runs[len(runs)-1]
		state := "green"
		if newest.result != "SUCCESS" {
			state = "red"
		}
		count := 0
		for i := len(runs) - 1; i >= 0; i-- {
			isPass := runs[i].result == "SUCCESS"
			if (state == "green") == isPass {
				count++
			} else {
				break
			}
		}
		info.Streak = Streak{Count: count, State: state}

		info.LatestRun = &RunInfo{
			ID:        newest.id,
			URL:       fmt.Sprintf("%s%s%s/%s", prowBaseURL, gcsLogsPrefix, jd.Name, newest.id),
			Timestamp: newest.started.UTC().Format(time.RFC3339),
			Result:    newest.result,
			DurationS: newest.finished.Sub(newest.started).Seconds(),
		}

		if newest.result != "SUCCESS" {
			info.LatestRun.Error = fetchStepError(jd, newest.id, g)
		}
	}

	slog.Info("ops job", "job", jd.Short, "runs", info.Runs, "pass", info.Passes, "fail", info.Failures, "streak", fmt.Sprintf("%d %s", info.Streak.Count, info.Streak.State))
	return info
}

// fetchStepError reads the step-level build-log.txt (which contains the
// actual error) rather than the outer CI wrapper log. Navigates down
// artifacts/{step-name}/{step-ref}/build-log.txt. Falls back to the
// outer build-log.txt if no step-level log is found.
func fetchStepError(jd jobDef, runID string, g gcs.Fetcher) string {
	artifactsPrefix := fmt.Sprintf("%s%s/%s/artifacts/", gcsLogsPrefix, jd.Name, runID)
	dirs, _, err := g.ListDir(artifactsPrefix)
	if err == nil {
		for _, stepDir := range dirs {
			if strings.HasSuffix(stepDir, "/build-resources/") {
				continue
			}
			subDirs, _, err := g.ListDir(stepDir)
			if err != nil {
				continue
			}
			for _, refDir := range subDirs {
				logPath := refDir + "build-log.txt"
				if data, err := g.Artifact(logPath); err == nil {
					return extractTail(string(data), 8, 500)
				}
			}
		}
	}
	logPath := fmt.Sprintf("%s%s/%s/build-log.txt", gcsLogsPrefix, jd.Name, runID)
	if data, err := g.Artifact(logPath); err == nil {
		return extractTail(string(data), 5, 500)
	}
	return ""
}

// extractTail returns the last N non-empty lines of s, capped to maxLen.
func extractTail(s string, lines, maxLen int) string {
	all := strings.Split(strings.TrimSpace(s), "\n")
	var nonEmpty []string
	for _, l := range all {
		l = strings.TrimSpace(l)
		if l != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) > lines {
		nonEmpty = nonEmpty[len(nonEmpty)-lines:]
	}
	result := strings.Join(nonEmpty, "\n")
	if len(result) > maxLen {
		result = result[len(result)-maxLen:]
	}
	return result
}

// Print writes a compact summary of ops job health to w.
func (r *Result) Print(w io.Writer) {
	fmt.Fprintf(w, "# Ops — %d days\n\n", r.Days)

	if len(r.Jobs) == 0 {
		fmt.Fprintln(w, "No ops jobs found.")
		return
	}

	for _, j := range r.Jobs {
		fmt.Fprintf(w, "%s (%s): %d/%d pass, %d %s streak\n",
			j.Short, j.Env, j.Passes, j.Runs, j.Streak.Count, j.Streak.State)
		if j.Streak.State == "red" && j.LatestRun != nil && j.LatestRun.Error != "" {
			for _, line := range strings.Split(j.LatestRun.Error, "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
}
