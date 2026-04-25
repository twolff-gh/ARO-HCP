package main

import (
	"testing"
)

func TestBuildRunContext(t *testing.T) {
	cases := []struct {
		name        string
		summary     RunSummary
		wantEnv     string
		wantPresub  bool
	}{
		{
			name:       "integration periodic",
			summary:    RunSummary{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel"},
			wantEnv:    "int",
			wantPresub: false,
		},
		{
			name:       "stage periodic",
			summary:    RunSummary{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-stage-e2e-parallel"},
			wantEnv:    "stg",
			wantPresub: false,
		},
		{
			name:       "prod periodic",
			summary:    RunSummary{Name: "periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel"},
			wantEnv:    "prod",
			wantPresub: false,
		},
		{
			name:       "presubmit",
			summary:    RunSummary{Name: "pull-ci-Azure-ARO-HCP-main-e2e-parallel", URL: "https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4500/pull-ci-Azure-ARO-HCP-main-e2e-parallel/123"},
			wantEnv:    "dev",
			wantPresub: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := buildRunContext(&tc.summary)
			if ctx.Env != tc.wantEnv {
				t.Errorf("env: got %q, want %q", ctx.Env, tc.wantEnv)
			}
			if ctx.IsPresubmit != tc.wantPresub {
				t.Errorf("is_presubmit: got %v, want %v", ctx.IsPresubmit, tc.wantPresub)
			}
		})
	}
}

func TestOutputTailBudget(t *testing.T) {
	cases := []struct {
		failed int
		want   int
	}{
		{0, budgetSmallTailLines},
		{5, budgetSmallTailLines},
		{6, budgetMediumTailLines},
		{20, budgetMediumTailLines},
		{21, 0},
		{100, 0},
	}
	for _, tc := range cases {
		got := outputTailBudget(tc.failed)
		if got != tc.want {
			t.Errorf("outputTailBudget(%d) = %d, want %d", tc.failed, got, tc.want)
		}
	}
}

func TestPerTestBudget(t *testing.T) {
	// Few failures: all tests returned
	result := &TriageResult{
		Errors: []ErrorGroup{
			{Tests: []string{"testA", "testB"}},
		},
	}
	names := perTestBudget(result)
	if len(names) != 2 {
		t.Errorf("small failure count: expected 2 tests, got %d", len(names))
	}

	// Moderate failures: one representative per group
	var groups []ErrorGroup
	for i := range 4 {
		tests := make([]string, 4)
		for j := range 4 {
			tests[j] = "test_" + string(rune('A'+i*4+j))
		}
		groups = append(groups, ErrorGroup{Tests: tests})
	}
	result = &TriageResult{Errors: groups}
	names = perTestBudget(result)
	if len(names) != 4 {
		t.Errorf("moderate failures: expected 4 representatives, got %d", len(names))
	}

	// Large failures: skip
	largeGroup := ErrorGroup{Tests: make([]string, 30)}
	for i := range 30 {
		largeGroup.Tests[i] = "test_" + string(rune('A'+i%26)) + "_" + string(rune('0'+i/26))
	}
	result = &TriageResult{Errors: []ErrorGroup{largeGroup}}
	names = perTestBudget(result)
	if len(names) != 0 {
		t.Errorf("large failures: expected 0, got %d", len(names))
	}
}

func TestBuildCoverage(t *testing.T) {
	result := &TriageResult{
		Scale:   FailureScale{HasTestResults: true},
		Steps:   []StepTiming{{Name: "step1"}},
		Metrics: &MetricsExtract{MaxLeaseAcqSec: 2.5},
		Podinfo: &PodinfoSummary{OOMDetected: true},
		Events:  &EventsSummary{CiJobFailed: true},
		Pool:    &PoolSummary{Contention: []string{"state: busy"}},
		Azure:   []AzureTestSummary{{ResponseErrors: map[string]int{"AuthError": 3}}},
		Errors: []ErrorGroup{
			{IsShortError: true, TestCount: 2},
			{IsCrashDump: true, TestCount: 1},
		},
	}

	cov := buildCoverage(result, true, false)

	if !cov.HasTestResults {
		t.Error("expected HasTestResults")
	}
	if !cov.HasStepGraph {
		t.Error("expected HasStepGraph")
	}
	if !cov.OOMDetected {
		t.Error("expected OOMDetected")
	}
	if !cov.CiJobFailed {
		t.Error("expected CiJobFailed")
	}
	if !cov.PoolContention {
		t.Error("expected PoolContention")
	}
	if cov.AzureErrorCount != 3 {
		t.Errorf("expected AzureErrorCount=3, got %d", cov.AzureErrorCount)
	}
	if cov.MaxLeaseAcqSec == nil || *cov.MaxLeaseAcqSec != 2.5 {
		t.Error("expected MaxLeaseAcqSec=2.5")
	}
	if cov.ShortErrorTests != 2 {
		t.Errorf("expected ShortErrorTests=2, got %d", cov.ShortErrorTests)
	}
	if cov.CrashDumpTests != 1 {
		t.Errorf("expected CrashDumpTests=1, got %d", cov.CrashDumpTests)
	}
	if !cov.HasAzureLogs {
		t.Error("expected HasAzureLogs when azureFetched=true")
	}
	if cov.HasTimingData {
		t.Error("expected !HasTimingData when timingFetched=false")
	}
}

func TestFailingTestNames(t *testing.T) {
	result := &TriageResult{
		Errors: []ErrorGroup{
			{Tests: []string{"testA", "testB"}},
			{Tests: []string{"testB", "testC"}},
		},
	}
	names := result.failingTestNames()
	if len(names) != 3 {
		t.Errorf("expected 3 unique names, got %d: %v", len(names), names)
	}
}
