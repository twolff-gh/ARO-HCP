package main

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

const (
	maxInnermostCauseLen     = 300 // truncation limit for innermost cause text
	maxErrorDisplayLen       = 1000
	minFailuresForRegrouping = 5  // minimum failed tests before trying coarser grouping
	maxSingletonPct          = 70 // above this % singletons, try coarser grouping
	crashDumpMinLen          = 500
	crashDumpPointerRatio    = 5 // raw must lose >1/ratio of length when stripped
	diagnosticEmptyLen       = 40
	causePrefixLen           = 60
	sigPrefixLen             = 40
)

// FailureScale describes the structural shape of failures in a run.
// Purely count-based — no content interpretation.
type FailureScale struct {
	FailedTestCount   int  `json:"failed_test_count"`
	TotalTestCount    int  `json:"total_test_count"`
	UniqueErrorGroups int  `json:"unique_error_groups"`
	LargestGroupPct   int  `json:"largest_group_pct"`
	IsCascade         bool `json:"is_cascade"`
	HasTestResults    bool `json:"has_test_results"`
}

// ErrorGroup is a cluster of tests sharing the same normalized error signature.
type ErrorGroup struct {
	Signature         string           `json:"signature"`
	Error             string           `json:"error"`
	TestCount         int              `json:"test_count"`
	Tests             []string         `json:"tests"`
	MedianDurationSec float64          `json:"median_duration_seconds,omitempty"`
	SourceFile        string           `json:"source_file,omitempty"`
	InnermostCause    string           `json:"innermost_cause,omitempty"`
	OutputTails       []OutputTailEntry `json:"output_tails,omitempty"`
	IsShortError      bool             `json:"is_short_error,omitempty"`
	IsCrashDump       bool             `json:"is_crash_dump,omitempty"`
}

// OutputTailEntry pairs a test name with the last N lines of its output.
type OutputTailEntry struct {
	TestName string   `json:"test_name"`
	Lines    []string `json:"lines"`
}

type classifyEntry struct {
	test    extensionTestResult
	cleaned string
	sig     string
	cause   string
	source  string
}

var sourceFileRe = regexp.MustCompile(`\[([^\]]+\.go:\d+)\]`)

// extractSourceFile returns the first Go source location (file.go:line) from error text.
func extractSourceFile(errText string) string {
	m := sourceFileRe.FindStringSubmatch(errText)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// extractInnermostCause returns the deepest "caused by:" or ", error:" segment,
// which strips test-specific identifiers like cluster names and resource groups.
func extractInnermostCause(errText string) string {
	parts := strings.Split(errText, "caused by:")
	if len(parts) > 1 {
		innermost := strings.TrimSpace(parts[len(parts)-1])
		if len(innermost) > maxInnermostCauseLen {
			innermost = innermost[:maxInnermostCauseLen]
		}
		return innermost
	}
	parts = strings.Split(errText, ", error:")
	if len(parts) > 1 {
		innermost := strings.TrimSpace(parts[len(parts)-1])
		if len(innermost) > maxInnermostCauseLen {
			innermost = innermost[:maxInnermostCauseLen]
		}
		return innermost
	}
	return ""
}

// classifyErrors groups failed test results by normalized error signature
// and computes the FailureScale. This is purely structural — groups by
// text similarity, not by meaning.
//
// Grouping uses a two-pass approach: first try the full normalized error.
// If that produces too many singleton groups (>80% singletons with 10+
// failures), fall back to grouping by innermost cause — the deepest
// "caused by:" segment, which strips test-specific identifiers like
// cluster names and resource groups.
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
		cleaned := stripGoPointers(t.Error)
		entries[i] = classifyEntry{
			test:    t,
			cleaned: cleaned,
			sig:     normalizeForSimilarity(cleaned),
			cause:   extractInnermostCause(t.Error),
			source:  extractSourceFile(t.Error),
		}
	}

	// First pass: group by full normalized signature
	groups := groupEntries(entries, func(e classifyEntry) string { return e.sig })

	// If too many singletons and enough failures to matter, try
	// progressively coarser grouping strategies until groups form.
	// Each strategy is structural — it uses a shorter key, not content matching.
	singletons := 0
	for _, g := range groups {
		if len(g) == 1 {
			singletons++
		}
	}
	if len(failed) >= minFailuresForRegrouping && singletons*100/len(groups) > maxSingletonPct {
		strategies := []struct {
			name  string
			keyFn func(classifyEntry) string
		}{
			{"innermost_cause", func(e classifyEntry) string {
				if e.cause != "" {
					return normalizeForSimilarity(e.cause)
				}
				return e.sig
			}},
			{"cause_prefix", func(e classifyEntry) string {
				cause := normalizeForSimilarity(e.cause)
				if len(cause) > causePrefixLen {
					cause = cause[:causePrefixLen]
				}
				if cause != "" {
					return cause
				}
				sig := e.sig
				if len(sig) > causePrefixLen {
					sig = sig[:causePrefixLen]
				}
				return sig
			}},
			{"sig_prefix", func(e classifyEntry) string {
				sig := e.sig
				if len(sig) > sigPrefixLen {
					sig = sig[:sigPrefixLen]
				}
				return sig
			}},
		}

		for _, strat := range strategies {
			candidate := groupEntries(entries, strat.keyFn)
			candidateSingletons := 0
			for _, g := range candidate {
				if len(g) == 1 {
					candidateSingletons++
				}
			}
			if len(candidate) < len(groups) && candidateSingletons < singletons {
				groups = candidate
				singletons = candidateSingletons
			}
			if singletons*100/max(len(groups), 1) <= maxSingletonPct {
				break
			}
		}
	}

	type bucket struct {
		sig       string
		repr      string
		tests     []string
		entries   []classifyEntry
		durations []float64
		source    string
		cause     string
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
	scale.IsCascade = scale.LargestGroupPct >= 70 && len(failed) >= 10

	result := make([]ErrorGroup, 0, len(bucketList))
	for _, b := range bucketList {
		// Detect crash dumps by comparing raw vs pointer-stripped text.
		// b.repr is already stripped, so we check the original entries.
		isCrashDump := false
		for _, e := range b.entries {
			raw := e.test.Error
			if len(raw) > crashDumpMinLen && (len(raw)-len(e.cleaned))*crashDumpPointerRatio > len(raw) {
				isCrashDump = true
				break
			}
		}
		eg := ErrorGroup{
			Signature:         b.sig,
			Error:             truncateLine(b.repr, maxErrorDisplayLen),
			TestCount:         len(b.tests),
			Tests:             b.tests,
			MedianDurationSec: medianFloat(b.durations),
			SourceFile:        b.source,
			InnermostCause:    b.cause,
			IsShortError:      isDiagnosticallyEmpty(b.repr),
			IsCrashDump:       isCrashDump,
		}
		if outputTailLines > 0 {
			for _, e := range b.entries {
				if outputTailLines <= 10 && !isDiagnosticallyEmpty(e.test.Error) {
					continue
				}
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

// groupEntries partitions classify entries by the key function into a map.
func groupEntries(entries []classifyEntry, keyFn func(classifyEntry) string) map[string][]classifyEntry {
	groups := map[string][]classifyEntry{}
	for _, e := range entries {
		key := keyFn(e)
		groups[key] = append(groups[key], e)
	}
	return groups
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

// isDiagnosticallyEmpty returns true if the error text is too short to be useful for triage.
func isDiagnosticallyEmpty(errText string) bool {
	return len(strings.TrimSpace(errText)) < diagnosticEmptyLen
}

// extractOutputTail returns the last n lines of output, or all lines if fewer than n.
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
