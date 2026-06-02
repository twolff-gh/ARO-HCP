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

// triage_input.go renders markdown optimized for LLM triage.
// Output is a summary table (structured signals) followed by
// all errors for every failing test.
package fleet

import (
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	failPrefixRe  = regexp.MustCompile(`^fail \[github\.com/[^\]]+\]:\s*`)
	ginkgoFrameRe = regexp.MustCompile(`(?i)unexpected error:\s*`)
	goErrorTypeRe = regexp.MustCompile(`<\*?[a-zA-Z._]+\s*(?:\|\s*0x[0-9a-f]+)?>:\s*`)
	occurredRe    = regexp.MustCompile(`\s*\.\.\.\s*occurred\s*`)

	// dedupKey patterns — strip instance identifiers that vary per run
	// but carry zero diagnostic value.
	uuidRe       = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	quotedRe     = regexp.MustCompile(`"[^"]*"`)
	timestampRe  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[0-9Z.+:-]*`)
	countInMsgRe = regexp.MustCompile(`count is '\d+'`)
	armPathRe    = regexp.MustCompile(`/subscriptions/[^\s]+`)
	rgRefRe      = regexp.MustCompile(`(resourcegroup|resource group)[=\s]+["]?[a-zA-Z0-9_-]+["]?`)
	clusterRefRe = regexp.MustCompile(`(cluster|nodepool|hcpCluster)[=\s]+["]?[a-zA-Z0-9_-]+["]?`)
	ocpVersionRe = regexp.MustCompile(`\b\d+\.\d+\.\d+(-rc\.\d+)?\b`)
)

// WriteTriageInput writes triage-ready markdown: a summary table
// of all failing tests followed by all errors for each test.
func WriteTriageInput(w io.Writer, r *Result) {
	writeHeader(w, r)
	if len(r.FailingTests) == 0 {
		fmt.Fprintf(w, "\nNo failing tests.\n")
		return
	}

	writeTable(w, r.FailingTests)
	writeErrors(w, r.FailingTests)
}

func writeTable(w io.Writer, tests []FailingTest) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| # | Test | Pre | Per | PRs | Pass% | Pool | Wait |")
	fmt.Fprintln(w, "|---|------|-----|-----|-----|-------|------|------|")
	for i, ft := range tests {
		pool := ""
		if ft.PoolRetries > 0 {
			pool = fmt.Sprintf("%d", ft.PoolRetries)
		}
		wait := ""
		if ft.PoolWaitS > 0 {
			wait = fmt.Sprintf("%ds", int(ft.PoolWaitS))
		}
		fmt.Fprintf(w, "| %d | %s | %d | %d | %s | %.1f | %s | %s |\n",
			i+1, ft.Test, ft.PreHits, ft.PeriodicHits, formatPRs(ft.PRNumbers), ft.PassRate, pool, wait)
	}
}

func formatPRs(prs []int) string {
	if len(prs) == 0 {
		return ""
	}
	parts := make([]string, len(prs))
	for i, pr := range prs {
		parts[i] = fmt.Sprintf("#%d", pr)
	}
	return strings.Join(parts, ",")
}

func writeErrors(w io.Writer, tests []FailingTest) {
	fmt.Fprintf(w, "\n## Errors\n")
	for i, ft := range tests {
		fmt.Fprintf(w, "\n### %d. %s (%d pre + %d per, %.1f%%)\n", i+1, ft.Test, ft.PreHits, ft.PeriodicHits, ft.PassRate)
		for _, s := range ft.ErrorSamples {
			if len(s.URLs) > 0 {
				fmt.Fprintf(w, "Run: %s\n", s.URLs[0])
			}
			fmt.Fprintf(w, "```\n%s\n```\n", cleanError(s.Text))
			if s.Count > 1 {
				fmt.Fprintf(w, "(x%d)\n", s.Count)
			}
		}
	}
}

func cleanError(s string) string {
	s = strings.TrimSpace(s)
	s = failPrefixRe.ReplaceAllString(s, "")
	s = ginkgoFrameRe.ReplaceAllString(s, "")
	s = goErrorTypeRe.ReplaceAllString(s, "")
	s = occurredRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// dedupKey strips instance-specific identifiers from an error so that
// structurally identical errors from different runs produce the same key.
// Only strips guaranteed-noise tokens: UUIDs, quoted strings (resource
// names), resource group paths, timestamps, and varying counts.
// Does NOT normalize error semantics — the LLM does that.
func dedupKey(s string) string {
	s = cleanError(s)
	s = uuidRe.ReplaceAllString(s, "*")
	s = quotedRe.ReplaceAllString(s, `"*"`)
	s = armPathRe.ReplaceAllString(s, "/subscriptions/*")
	s = rgRefRe.ReplaceAllString(s, "resourcegroup=*")
	s = clusterRefRe.ReplaceAllString(s, "cluster=*")
	s = ocpVersionRe.ReplaceAllString(s, "*")
	s = timestampRe.ReplaceAllString(s, "*")
	s = countInMsgRe.ReplaceAllString(s, "count is '*'")
	return s
}

func writeHeader(w io.Writer, r *Result) {
	fmt.Fprintf(w, "# Fleet: %s, %d days\n\n", r.Env, r.Days)

	streak := formatStreak(r.Health.Streak)
	if r.Health.TotalRuns14Day > 0 {
		fmt.Fprintf(w, "Pass rate: %.1f%% (%dd) / %.1f%% (14d) | Runs: %d (%dd) / %d (14d) | %s\n",
			r.Health.PassRate, r.Days,
			r.Health.PassRate14Day,
			r.Health.TotalRuns, r.Days,
			r.Health.TotalRuns14Day,
			streak)
	} else {
		fmt.Fprintf(w, "Pass rate: %.1f%% | Runs: %d | %s\n",
			r.Health.PassRate, r.Health.TotalRuns, streak)
	}

	if r.RunSummary != nil {
		s := r.RunSummary
		fmt.Fprintf(w, "Failed runs: %d (%d PR, %d periodic)\n",
			s.TotalRuns, s.PRRuns, s.PeriodicRuns)
	}
}

func formatStreak(s Streak) string {
	if s.Count == 0 {
		return "Streak: 0"
	}
	base := fmt.Sprintf("Streak: %d %s", s.Count, s.State)
	if s.Since != "" && s.State == "red" && len(s.Since) >= 10 {
		base += " since " + s.Since[:10]
	}
	return base
}
