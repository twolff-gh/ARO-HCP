package main

import (
	"testing"
)

func TestClassifyErrors_LargeGroup(t *testing.T) {
	tests := make([]extensionTestResult, 12)
	for i := range tests {
		tests[i] = extensionTestResult{
			Name:   "test_" + string(rune('A'+i)),
			Result: "failed",
			Error:  "context deadline exceeded waiting for condition: timed out",
		}
	}
	groups, scale := classifyErrors(tests, 0)

	if scale.FailedTestCount != 12 {
		t.Errorf("expected 12 failed, got %d", scale.FailedTestCount)
	}
	if scale.LargestGroupPct != 100 {
		t.Errorf("expected 100%% largest group, got %d%%", scale.LargestGroupPct)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 error group, got %d", len(groups))
	}
	if groups[0].TestCount != 12 {
		t.Errorf("expected group with 12 tests, got %d", groups[0].TestCount)
	}
}

func TestClassifyErrors_DistinctErrors(t *testing.T) {
	tests := []extensionTestResult{
		{Name: "test_A", Result: "failed", Error: "timeout waiting for cluster"},
		{Name: "test_B", Result: "failed", Error: "resource group not found"},
		{Name: "test_C", Result: "failed", Error: "pod OOMKilled during install"},
	}
	_, scale := classifyErrors(tests, 0)

	if scale.LargestGroupPct > 34 {
		t.Errorf("3 distinct errors should each be ~33%%, got largest=%d%%", scale.LargestGroupPct)
	}
	if scale.FailedTestCount != 3 {
		t.Errorf("expected 3 failed, got %d", scale.FailedTestCount)
	}
}

func TestClassifyErrors_PassingOnly(t *testing.T) {
	tests := []extensionTestResult{
		{Name: "test_A", Result: "passed"},
		{Name: "test_B", Result: "passed"},
	}
	groups, scale := classifyErrors(tests, 0)
	if scale.FailedTestCount != 0 {
		t.Errorf("expected 0 failed, got %d", scale.FailedTestCount)
	}
	if len(groups) != 0 {
		t.Errorf("expected no groups, got %d", len(groups))
	}
}

func TestExtractInnermostCause(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"something failed, caused by: RoleAssignmentLimitExceeded", "RoleAssignmentLimitExceeded"},
		{"layer1, caused by: layer2, caused by: deepest error", "deepest error"},
		{"something failed, error: connection refused", "connection refused"},
		{"no cause chain here", ""},
	}
	for _, tc := range cases {
		got, _ := extractInnermostCause(tc.input)
		if got != tc.want {
			t.Errorf("extractInnermostCause(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestExtractSourceFile(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"fail [cluster_test.go:42]: unexpected error", "cluster_test.go:42"},
		{"no source location", ""},
	}
	for _, tc := range cases {
		got := extractSourceFile(tc.input)
		if got != tc.want {
			t.Errorf("extractSourceFile(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMedianFloat(t *testing.T) {
	cases := []struct {
		vals []float64
		want float64
	}{
		{nil, 0},
		{[]float64{5}, 5},
		{[]float64{1, 3}, 2},
		{[]float64{3, 1, 2}, 2},
	}
	for _, tc := range cases {
		got := medianFloat(tc.vals)
		if got != tc.want {
			t.Errorf("medianFloat(%v) = %v, want %v", tc.vals, got, tc.want)
		}
	}
}
