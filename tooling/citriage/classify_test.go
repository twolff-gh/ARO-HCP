package main

import (
	"testing"
)

func TestClassifyErrors_Cascade(t *testing.T) {
	// 10+ failures with one dominant error => cascade
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
	if !scale.IsCascade {
		t.Error("expected cascade when 12 tests share one error")
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 error group, got %d", len(groups))
	}
	if groups[0].TestCount != 12 {
		t.Errorf("expected group with 12 tests, got %d", groups[0].TestCount)
	}
}

func TestClassifyErrors_NotCascade(t *testing.T) {
	// 3 different errors across 3 tests => not cascade
	tests := []extensionTestResult{
		{Name: "test_A", Result: "failed", Error: "timeout waiting for cluster"},
		{Name: "test_B", Result: "failed", Error: "resource group not found"},
		{Name: "test_C", Result: "failed", Error: "pod OOMKilled during install"},
	}
	_, scale := classifyErrors(tests, 0)

	if scale.IsCascade {
		t.Error("3 distinct errors across 3 tests should not be cascade")
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

func TestClassifyErrors_RegroupingOnSingletons(t *testing.T) {
	// Many failures with different UUIDs/timestamps in the error but same innermost cause.
	// First-pass grouping produces singletons; regrouping should coalesce.
	var tests []extensionTestResult
	for i := range 10 {
		tests = append(tests, extensionTestResult{
			Name:   "test_" + string(rune('A'+i)),
			Result: "failed",
			Error:  "failed to create resource group rg-" + string(rune('a'+i)) + "-abc123, caused by: RoleAssignmentLimitExceeded",
		})
	}

	groups, scale := classifyErrors(tests, 0)

	if scale.FailedTestCount != 10 {
		t.Errorf("expected 10 failed, got %d", scale.FailedTestCount)
	}
	// After regrouping by innermost cause, all should land in one group
	if len(groups) > 3 {
		t.Errorf("expected regrouping to reduce groups, got %d groups", len(groups))
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
		got := extractInnermostCause(tc.input)
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

func TestIsDiagnosticallyEmpty(t *testing.T) {
	if !isDiagnosticallyEmpty("err") {
		t.Error("3-char error should be diagnostically empty")
	}
	if isDiagnosticallyEmpty("this is a sufficiently long error message for diagnosis") {
		t.Error("long error should not be diagnostically empty")
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
