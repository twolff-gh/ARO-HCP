package main

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

const (
	maxInnermostCauseLen = 600
)

// FailureScale describes the structural shape of failures in a run.
type FailureScale struct {
	FailedTestCount   int  `json:"failed_test_count"`
	TotalTestCount    int  `json:"total_test_count"`
	UniqueErrorGroups int  `json:"unique_error_groups"`
	LargestGroupPct   int  `json:"largest_group_pct"`
	HasTestResults    bool `json:"has_test_results"`
}

// ErrorGroup is a cluster of tests sharing the same normalized error signature.
type ErrorGroup struct {
	Signature              string            `json:"signature"`
	Error                  string            `json:"error"`
	TestCount              int               `json:"test_count"`
	Tests                  []string          `json:"tests"`
	MedianDurationSec      float64           `json:"median_duration_seconds,omitempty"`
	SourceFile             string            `json:"source_file,omitempty"`
	InnermostCause         string            `json:"innermost_cause,omitempty"`
	InnermostCauseTruncated bool             `json:"innermost_cause_truncated,omitempty"`
	OutputTails            []OutputTailEntry `json:"output_tails,omitempty"`
}

type OutputTailEntry struct {
	TestName string   `json:"test_name"`
	Lines    []string `json:"lines"`
}

type classifyEntry struct {
	test           extensionTestResult
	cleaned        string
	sig            string
	cause          string
	causeTruncated bool
	source         string
}

var sourceFileRe = regexp.MustCompile(`\[([^\]]+\.go:\d+)\]`)

func extractSourceFile(errText string) string {
	m := sourceFileRe.FindStringSubmatch(errText)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func extractInnermostCause(errText string) (string, bool) {
	parts := strings.Split(errText, "caused by:")
	if len(parts) > 1 {
		innermost := strings.TrimSpace(parts[len(parts)-1])
		if len(innermost) > maxInnermostCauseLen {
			return innermost[:maxInnermostCauseLen], true
		}
		return innermost, false
	}
	parts = strings.Split(errText, ", error:")
	if len(parts) > 1 {
		innermost := strings.TrimSpace(parts[len(parts)-1])
		if len(innermost) > maxInnermostCauseLen {
			return innermost[:maxInnermostCauseLen], true
		}
		return innermost, false
	}
	return "", false
}

// classifyErrors groups failed test results by normalized error signature
// and computes the FailureScale.
func classifyErrors(tests []extensionTestResult, outputTailLines int) ([]ErrorGroup, FailureScale) {
	scale := FailureScale{
		TotalTestCount: len(tests),
		HasTestResults: true,
	}

	var failed []extensionTestResult
	for _, t := range tests {
		if t.Result == "failed" {
			failed = append(failed, t)
		}
	}
	scale.FailedTestCount = len(failed)

	if len(failed) == 0 {
		return nil, scale
	}

	entries := make([]classifyEntry, len(failed))
	for i, t := range failed {
		cause, causeTrunc := extractInnermostCause(t.Error)
		entries[i] = classifyEntry{
			test:           t,
			cleaned:        t.Error,
			sig:            normalizeForSimilarity(stripGoPointers(t.Error)),
			cause:          cause,
			causeTruncated: causeTrunc,
			source:         extractSourceFile(t.Error),
		}
	}

	groups := map[string][]classifyEntry{}
	for _, e := range entries {
		groups[e.sig] = append(groups[e.sig], e)
	}

	type bucket struct {
		sig            string
		repr           string
		tests          []string
		entries        []classifyEntry
		durations      []float64
		source         string
		cause          string
		causeTruncated bool
	}

	var bucketList []*bucket
	for key, ents := range groups {
		b := &bucket{sig: key}
		for _, e := range ents {
			b.tests = append(b.tests, e.test.Name)
			b.entries = append(b.entries, e)
			if e.test.Duration > 0 && e.test.StartTime != e.test.EndTime {
				b.durations = append(b.durations, float64(e.test.Duration)/1000.0)
			}
			if len(e.cleaned) > len(b.repr) {
				b.repr = e.cleaned
			}
			if b.source == "" {
				b.source = e.source
			}
			if b.cause == "" {
				b.cause = e.cause
				b.causeTruncated = e.causeTruncated
			}
		}
		bucketList = append(bucketList, b)
	}

	maxGroupSize := 0
	for _, b := range bucketList {
		if len(b.tests) > maxGroupSize {
			maxGroupSize = len(b.tests)
		}
	}

	scale.UniqueErrorGroups = len(bucketList)
	scale.LargestGroupPct = maxGroupSize * 100 / len(failed)

	result := make([]ErrorGroup, 0, len(bucketList))
	for _, b := range bucketList {
		eg := ErrorGroup{
			Signature:              b.sig,
			Error:                  b.repr,
			TestCount:              len(b.tests),
			Tests:                  b.tests,
			MedianDurationSec:      medianFloat(b.durations),
			SourceFile:             b.source,
			InnermostCause:         b.cause,
			InnermostCauseTruncated: b.causeTruncated,
		}
		if outputTailLines > 0 {
			for _, e := range b.entries {
				if tail := extractOutputTail(e.test.Output, outputTailLines); len(tail) > 0 {
					eg.OutputTails = append(eg.OutputTails, OutputTailEntry{
						TestName: e.test.Name,
						Lines:    tail,
					})
				}
			}
		}
		result = append(result, eg)
	}

	slices.SortFunc(result, func(a, b ErrorGroup) int {
		return cmp.Compare(b.TestCount, a.TestCount)
	})

	return result, scale
}

func medianFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}

func extractOutputTail(output string, n int) []string {
	if output == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
