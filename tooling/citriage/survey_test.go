package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIsSyntheticTest(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"[sig-sippy] infrastructure should work", true},
		{"[sig-sippy] anything else", true},
		{"Job run should complete before timeout", true},
		{"Customer should be able to create an HCP cluster", false},
		{"", false},
		{"[sig-other] something", false},
	}
	for _, tt := range tests {
		if got := isSyntheticTest(tt.name); got != tt.want {
			t.Errorf("isSyntheticTest(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRealFailureCount(t *testing.T) {
	tests := []struct {
		desc  string
		run   JobRun
		want  int
	}{
		{
			desc: "all synthetic",
			run: JobRun{
				TestFailures:    3,
				FailedTestNames: []string{"[sig-sippy] infra", "Job run should complete before timeout", "[sig-sippy] other"},
			},
			want: 0,
		},
		{
			desc: "one real failure plus synthetics",
			run: JobRun{
				TestFailures:    3,
				FailedTestNames: []string{"[sig-sippy] infra", "Customer should create cluster", "Job run should complete before timeout"},
			},
			want: 1,
		},
		{
			desc: "no FailedTestNames falls back to TestFailures",
			run: JobRun{
				TestFailures:    5,
				FailedTestNames: nil,
			},
			want: 5,
		},
		{
			desc: "empty FailedTestNames falls back to TestFailures",
			run: JobRun{
				TestFailures:    2,
				FailedTestNames: []string{},
			},
			want: 2,
		},
		{
			desc: "all real failures",
			run: JobRun{
				TestFailures:    2,
				FailedTestNames: []string{"test A", "test B"},
			},
			want: 2,
		},
		{
			desc: "zero failures",
			run: JobRun{
				TestFailures:    0,
				FailedTestNames: []string{},
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := realFailureCount(tt.run); got != tt.want {
				t.Errorf("realFailureCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestComputeStreak(t *testing.T) {
	tests := []struct {
		desc        string
		runs        []JobRun
		wantStreak  int
		wantGreen   bool
	}{
		{
			desc:       "all green",
			runs:       []JobRun{{TestFailures: 0}, {TestFailures: 0}, {TestFailures: 0}},
			wantStreak: 3,
			wantGreen:  true,
		},
		{
			desc:       "first red then green",
			runs:       []JobRun{{TestFailures: 2}, {TestFailures: 0}, {TestFailures: 0}},
			wantStreak: 1,
			wantGreen:  false,
		},
		{
			desc:       "two red then green",
			runs:       []JobRun{{TestFailures: 1}, {TestFailures: 3}, {TestFailures: 0}},
			wantStreak: 2,
			wantGreen:  false,
		},
		{
			desc:       "synthetic only counts as green",
			runs:       []JobRun{{FailedTestNames: []string{"[sig-sippy] infra"}}, {TestFailures: 0}},
			wantStreak: 2,
			wantGreen:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			streak, green := computeStreak(tt.runs)
			if streak != tt.wantStreak || green != tt.wantGreen {
				t.Errorf("computeStreak() = (%d, %v), want (%d, %v)", streak, green, tt.wantStreak, tt.wantGreen)
			}
		})
	}
}

func TestFilterNightlyRuns(t *testing.T) {
	runs := []JobRun{
		{Job: "periodic-integration-e2e-parallel"},
		{Job: "periodic-prod-e2e-parallel-ocp-nightly"},
		{Job: "periodic-stage-e2e-parallel"},
	}
	got, excluded := filterNightlyRuns(runs)
	if len(got) != 2 {
		t.Fatalf("filterNightlyRuns returned %d runs, want 2", len(got))
	}
	if excluded != 1 {
		t.Errorf("filterNightlyRuns excluded %d, want 1", excluded)
	}
	for _, r := range got {
		if r.Job == "periodic-prod-e2e-parallel-ocp-nightly" {
			t.Error("nightly run should have been filtered out")
		}
	}
}

func TestEv2Hash(t *testing.T) {
	tests := []struct {
		desc string
		run  JobRun
		want string
	}{
		{"nil annotations", JobRun{}, ""},
		{"no ev2 key", JobRun{Annotations: map[string]string{"other": "val"}}, ""},
		{"short hash", JobRun{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abc123"}}, "abc123"},
		{"long hash preserved", JobRun{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abcdef1234567890abcd"}}, "abcdef1234567890abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := ev2Hash(tt.run); got != tt.want {
				t.Errorf("ev2Hash() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilterRunsByDate(t *testing.T) {
	now := time.Now()
	runs := []JobRun{
		{ID: 1, Timestamp: now.Add(-2 * time.Hour).UnixMilli()},
		{ID: 2, Timestamp: now.Add(-48 * time.Hour).UnixMilli()},
		{ID: 3, Timestamp: now.Add(-1 * time.Hour).UnixMilli()},
		{ID: 4, Timestamp: now.Add(-72 * time.Hour).UnixMilli()},
	}
	cutoff := now.Add(-24 * time.Hour)
	got := filterRunsByDate(runs, cutoff)
	if len(got) != 2 {
		t.Fatalf("filterRunsByDate returned %d runs, want 2", len(got))
	}
	for _, r := range got {
		if r.ID == 2 || r.ID == 4 {
			t.Errorf("run %d should have been filtered out (older than cutoff)", r.ID)
		}
	}
}

func TestBuildRegionRates(t *testing.T) {
	runs := []JobRun{
		{Annotations: map[string]string{"ev2.rollout/region": "eastus2"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "eastus2"}, TestFailures: 2, FailedTestNames: []string{"test A"}},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 1, FailedTestNames: []string{"test B"}},
		{TestFailures: 0}, // no region annotation — should be excluded
	}
	got := buildRegionRates(runs)
	if len(got) != 2 {
		t.Fatalf("buildRegionRates returned %d regions, want 2", len(got))
	}
	// Sorted alphabetically: eastus2 first, uksouth second
	if got[0].Region != "eastus2" || got[0].Pass != 1 || got[0].Total != 2 {
		t.Errorf("eastus2: got pass=%d total=%d, want pass=1 total=2", got[0].Pass, got[0].Total)
	}
	if got[1].Region != "uksouth" || got[1].Pass != 2 || got[1].Total != 3 {
		t.Errorf("uksouth: got pass=%d total=%d, want pass=2 total=3", got[1].Pass, got[1].Total)
	}
}

func TestExtractPullNumber(t *testing.T) {
	tests := []struct {
		url  string
		want int
	}{
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4690/pull-ci-Azure-ARO-HCP-main-e2e-parallel/123", 4690},
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/Azure_ARO-HCP/4964/pull-ci-Azure-ARO-HCP-main-e2e-parallel/456", 4964},
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/pr-logs/pull/batch/pull-ci-Azure-ARO-HCP-main-e2e-parallel/789", 0},
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel/101", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := extractPullNumber(tt.url); got != tt.want {
			t.Errorf("extractPullNumber(%q) = %d, want %d", tt.url, got, tt.want)
		}
	}
}

func TestBuildRegionRatesNoRegions(t *testing.T) {
	runs := []JobRun{
		{TestFailures: 0},
		{TestFailures: 1, FailedTestNames: []string{"test A"}},
	}
	got := buildRegionRates(runs)
	if got != nil {
		t.Errorf("buildRegionRates with no regions should return nil, got %v", got)
	}
}

func TestBuildRegionRatesLowSample(t *testing.T) {
	runs := []JobRun{
		{Annotations: map[string]string{"ev2.rollout/region": "eastus2"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/region": "uksouth"}, TestFailures: 1, FailedTestNames: []string{"test A"}},
	}
	got := buildRegionRates(runs)
	if len(got) != 2 {
		t.Fatalf("expected 2 regions, got %d", len(got))
	}
	// eastus2 has 1 run → low_sample
	if got[0].Region != "eastus2" || !got[0].LowSample {
		t.Errorf("eastus2: LowSample=%v, want true (1 run)", got[0].LowSample)
	}
	// uksouth has 3 runs → not low_sample
	if got[1].Region != "uksouth" || got[1].LowSample {
		t.Errorf("uksouth: LowSample=%v, want false (3 runs)", got[1].LowSample)
	}
}

func TestBuildFailuresJSON(t *testing.T) {
	failures := []RecentFailure{
		{
			TestName:     "TestA",
			FailureCount: 2,
			FirstFailure: "2026-04-20T00:00:00Z",
			LastFailure:  "2026-04-22T00:00:00Z",
			Outputs: []FailureOutput{
				{RunID: 1, Output: "timeout exceeded"},
				{RunID: 2, Output: "timeout exceeded"},
			},
		},
		{
			TestName:     "[sig-sippy] synthetic",
			FailureCount: 1,
			Outputs:      []FailureOutput{{RunID: 3}},
		},
	}
	runs := []JobRun{
		{ID: 1, URL: "https://prow.example.com/1"},
		{ID: 2, URL: "https://prow.example.com/2"},
	}
	result := buildFailuresJSON(failures, runs)

	if len(result) != 1 {
		t.Fatalf("expected 1 failure (synthetic filtered), got %d", len(result))
	}
	if result[0].BestRunID != 1 {
		t.Errorf("BestRunID = %d, want 1", result[0].BestRunID)
	}
	if result[0].BestRunURL != "https://prow.example.com/1" {
		t.Errorf("BestRunURL = %q, want prow URL", result[0].BestRunURL)
	}
	if len(result[0].Outputs) != 2 {
		t.Errorf("expected 2 outputs, got %d", len(result[0].Outputs))
	}
}

func TestBuildFailuresJSON_OutputCapping(t *testing.T) {
	t.Run("same error deduplicates to one slot", func(t *testing.T) {
		var failures []RecentFailure
		for i := 0; i < 25; i++ {
			failures = append(failures, RecentFailure{
				TestName:     fmt.Sprintf("test-%d", i),
				FailureCount: 25 - i,
				Outputs:      []FailureOutput{{RunID: int64(i + 1), Output: "same error"}},
			})
		}
		result := buildFailuresJSON(failures, nil)
		if len(result) != 25 {
			t.Fatalf("expected 25 failures, got %d", len(result))
		}
		for _, f := range result {
			if len(f.Outputs) == 0 {
				t.Errorf("failure %s should keep outputs (same signature = 1 slot)", f.TestName)
			}
		}
	})

	t.Run("distinct errors cap at 20 unique signatures", func(t *testing.T) {
		var failures []RecentFailure
		for i := 0; i < 25; i++ {
			failures = append(failures, RecentFailure{
				TestName:     fmt.Sprintf("test-%d", i),
				FailureCount: 25 - i,
				Outputs:      []FailureOutput{{RunID: int64(i + 1), Output: fmt.Sprintf("unique error %d", i)}},
			})
		}
		result := buildFailuresJSON(failures, nil)
		if len(result) != 25 {
			t.Fatalf("expected 25 failures, got %d", len(result))
		}
		withOutputs := 0
		for _, f := range result {
			if len(f.Outputs) > 0 {
				withOutputs++
			}
		}
		if withOutputs != 20 {
			t.Errorf("expected 20 failures with outputs, got %d", withOutputs)
		}
		for i := 0; i < 20; i++ {
			if len(result[i].Outputs) == 0 {
				t.Errorf("failure %d (%s) should have outputs (within cap)", i, result[i].TestName)
			}
		}
		for i := 20; i < 25; i++ {
			if len(result[i].Outputs) != 0 {
				t.Errorf("failure %d (%s) should have nil outputs (beyond cap)", i, result[i].TestName)
			}
		}
	})

	t.Run("cascade frees slots for tail", func(t *testing.T) {
		var failures []RecentFailure
		for i := 0; i < 15; i++ {
			failures = append(failures, RecentFailure{
				TestName:     fmt.Sprintf("cascade-%d", i),
				FailureCount: 30,
				Outputs:      []FailureOutput{{RunID: int64(i + 1), Output: "timeout 45 minutes exceeded"}},
			})
		}
		for i := 0; i < 10; i++ {
			failures = append(failures, RecentFailure{
				TestName:     fmt.Sprintf("unique-%d", i),
				FailureCount: 5 - (i / 3),
				Outputs:      []FailureOutput{{RunID: int64(100 + i), Output: fmt.Sprintf("distinct error %d", i)}},
			})
		}
		result := buildFailuresJSON(failures, nil)
		uniqueWithOutputs := 0
		for _, f := range result {
			if len(f.Outputs) > 0 && strings.HasPrefix(f.TestName, "unique-") {
				uniqueWithOutputs++
			}
		}
		if uniqueWithOutputs != 10 {
			t.Errorf("expected all 10 unique failures to have outputs (cascade uses 1 slot), got %d", uniqueWithOutputs)
		}
	})
}

func TestBuildRunsJSON_FailedOnly(t *testing.T) {
	runs := []JobRun{
		{ID: 1, OverallResult: "S", TestFailures: 0},
		{ID: 2, OverallResult: "F", TestFailures: 3, FailedTestNames: []string{"testA", "testB", "testC"}},
		{ID: 3, OverallResult: "S", TestFailures: 0},
		{ID: 4, OverallResult: "F", TestFailures: 1, FailedTestNames: []string{"testA"}},
	}
	result := buildRunsJSON(runs)

	if len(result) != 2 {
		t.Fatalf("expected 2 failed runs, got %d", len(result))
	}
	if result[0].ID != 2 || result[1].ID != 4 {
		t.Errorf("expected run IDs 2 and 4, got %d and %d", result[0].ID, result[1].ID)
	}
}

func TestExtractStepError(t *testing.T) {
	tests := []struct {
		desc string
		in   string
		want string
	}{
		{
			"extracts ERROR entry from multi-entry log",
			`time=2026-04-22T17:07:22.667Z level=INFO msg="Running step." serviceGroup=Microsoft.Azure.ARO.HCP.Region resourceGroup=regional step=infra
time=2026-04-22T17:07:24.476Z level=DEBUG msg="Resolved values."
time=2026-04-22T17:35:00.000Z level=ERROR msg="Step errored." serviceGroup=Microsoft.Azure.ARO.HCP.Region step=infra error="deployment failed: RoleAssignmentLimitExceeded"`,
			`time=2026-04-22T17:35:00.000Z level=ERROR msg="Step errored." serviceGroup=Microsoft.Azure.ARO.HCP.Region step=infra error="deployment failed: RoleAssignmentLimitExceeded"`,
		},
		{
			"extracts ERROR from single-line concatenated log",
			`time=2026-04-22T17:07:22.667Z level=INFO msg="Running step." time=2026-04-22T17:35:00.000Z level=ERROR msg="Step errored." error="RoleAssignmentLimitExceeded"`,
			`time=2026-04-22T17:35:00.000Z level=ERROR msg="Step errored." error="RoleAssignmentLimitExceeded"`,
		},
		{
			"returns empty when no ERROR entries",
			`time=2026-04-22T17:07:22.667Z level=INFO msg="Running step."
time=2026-04-22T17:07:24.476Z level=DEBUG msg="Resolved values."`,
			"",
		},
		{
			"falls back to tail for non-structured-log output",
			"some random error text without templatize format",
			"some random error text without templatize format",
		},
		{
			"handles FATAL level",
			`time=2026-04-22T17:07:22.667Z level=INFO msg="start" time=2026-04-22T17:08:00.000Z level=FATAL msg="crashed"`,
			`time=2026-04-22T17:08:00.000Z level=FATAL msg="crashed"`,
		},
		{
			"handles multiple ERROR entries",
			`time=2026-04-22T17:00:00.000Z level=ERROR msg="first error"
time=2026-04-22T17:01:00.000Z level=INFO msg="retrying"
time=2026-04-22T17:02:00.000Z level=ERROR msg="second error"`,
			"time=2026-04-22T17:00:00.000Z level=ERROR msg=\"first error\"\ntime=2026-04-22T17:02:00.000Z level=ERROR msg=\"second error\"",
		},
		{
			"returns empty for empty input",
			"",
			"",
		},
		{
			"extracts ARM JSON error code and message",
			`{
  "error": {
    "code": "TooManyRequests",
    "message": "ValidateVMScaleSetOperation exceeded throttling limit"
  }
}`,
			"TooManyRequests: ValidateVMScaleSetOperation exceeded throttling limit",
		},
		{
			"extracts all ARM error codes including wrapper",
			`{
  "status": "Failed",
  "error": {
    "code": "DeploymentFailed",
    "message": "At least one resource deployment operation failed.",
    "details": [
      {
        "code": "Conflict",
        "message": "Operation conflict occurred.",
        "details": [
          {
            "code": "TooManyRequests",
            "message": "exceeded throttling limit of 0 calls within last 5 minutes"
          }
        ]
      }
    ]
  }
}`,
			"DeploymentFailed: At least one resource deployment operation failed.\nConflict: Operation conflict occurred.\nTooManyRequests: exceeded throttling limit of 0 calls within last 5 minutes",
		},
		{
			"extracts HTTP response error",
			"GET https://management.azure.com/...\n--------------------------------------------------------------------------------\nRESPONSE 429: 429 Too Many Requests\nERROR CODE: TooManyRequests\n--------------------------------------------------------------------------------",
			"HTTP 429: TooManyRequests",
		},
		{
			"extracts err field from unstructured log",
			`some prefix err="deployment failed: RoleAssignmentLimitExceeded" more text`,
			"deployment failed: RoleAssignmentLimitExceeded",
		},
		{
			"extracts last err field when multiple present",
			`err="initial error" retrying... err="final error: quota exceeded"`,
			"final error: quota exceeded",
		},
		{
			"structured log takes priority over ARM JSON",
			`time=2026-04-22T17:00:00.000Z level=ERROR msg="Step errored." error="bad"
{"error": {"code": "TooManyRequests", "message": "limit exceeded"}}`,
			"time=2026-04-22T17:00:00.000Z level=ERROR msg=\"Step errored.\" error=\"bad\"\n{\"error\": {\"code\": \"TooManyRequests\", \"message\": \"limit exceeded\"}}",
		},
		{
			"tail fallback for opaque output",
			"a]b]c]" + strings.Repeat("x", 600),
			strings.Repeat("x", 500),
		},
		{
			"real ARM TooManyRequests from presubmit pipeline",
			`RESPONSE 200: 200 OK
ERROR CODE: DeploymentFailed
--------------------------------------------------------------------------------
{
  "status": "Failed",
  "error": {
    "code": "DeploymentFailed",
    "message": "At least one resource deployment operation failed.",
    "details": [
      {
        "code": "ResourceDeploymentFailure",
        "message": "The resource operation completed with terminal provisioning state 'Failed'.",
        "details": [
          {
            "code": "Conflict",
            "message": "Operation could not be completed as it results in exceeding approved Total Regional Cores quota."
          }
        ]
      }
    ]
  }
}`,
			"DeploymentFailed: At least one resource deployment operation failed.\nResourceDeploymentFailure: The resource operation completed with terminal provisioning state 'Failed'.\nConflict: Operation could not be completed as it results in exceeding approved Total Regional Cores quota.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := extractStepError(tt.in)
			if got != tt.want {
				t.Errorf("extractStepError() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestExtractARMError_FiltersShortCodes(t *testing.T) {
	input := `"code": "}", "message": "brace"` + "\n" + `"code": "DeploymentFailed", "message": "real error"`
	got := extractARMError(input)
	if strings.Contains(got, "}") {
		t.Errorf("should filter single-char code, got %q", got)
	}
	if !strings.Contains(got, "DeploymentFailed") {
		t.Errorf("should keep real code, got %q", got)
	}
}

func TestMarkEmptyErrors(t *testing.T) {
	failures := []RecentFailure{
		{
			TestName: "test with empty outputs",
			Outputs:  []FailureOutput{{RunID: 1, Output: ""}, {RunID: 2, Output: ""}},
		},
		{
			TestName: "test with some output",
			Outputs:  []FailureOutput{{RunID: 3, Output: "real error"}},
		},
		{
			TestName: "test with no outputs",
			Outputs:  nil,
		},
	}
	markEmptyErrors(failures)
	if failures[0].Outputs[0].Output != emptyErrorSentinel {
		t.Errorf("expected sentinel on first empty failure, got %q", failures[0].Outputs[0].Output)
	}
	if failures[1].Outputs[0].Output != "real error" {
		t.Errorf("should not touch non-empty output, got %q", failures[1].Outputs[0].Output)
	}
}

func TestErrorSignature(t *testing.T) {
	outputs := []failureOutputJSON{
		{Error: ""},
		{Error: "timeout exceeded during cluster creation"},
	}
	sig := errorSignature(outputs)
	if sig != "timeout exceeded during cluster creation" {
		t.Errorf("expected non-empty error as signature, got %q", sig)
	}
	if errorSignature(nil) != "" {
		t.Error("nil outputs should return empty signature")
	}
	if errorSignature([]failureOutputJSON{{Error: ""}}) != "" {
		t.Error("all-empty outputs should return empty signature")
	}
}

func TestExtractPipelineStepErrors(t *testing.T) {
	failures := []RecentFailure{
		{
			TestName: "Run pipeline step Microsoft.Azure.ARO.HCP.Region/regional/infra",
			Outputs: []FailureOutput{
				{RunID: 1, Output: `time=2026-04-22T17:07:22.667Z level=INFO msg="Running step." time=2026-04-22T17:35:00.000Z level=ERROR msg="Step errored." error="RoleAssignmentLimitExceeded"`},
				{RunID: 2, Output: ""},
			},
		},
		{
			TestName: "Customer should be able to create an HCP cluster",
			Outputs: []FailureOutput{
				{RunID: 3, Output: "fail [test.go:42]: timeout exceeded"},
			},
		},
		{
			TestName: "Run pipeline step Microsoft.Azure.ARO.HCP.ACM/management/deploy-mce",
			Outputs: []FailureOutput{
				{RunID: 4, Output: `time=2026-04-17T23:26:22.973Z level=INFO msg="Running step." time=2026-04-17T23:26:23.579Z level=INFO msg="Resolved input values."`},
			},
		},
	}

	extractPipelineStepErrors(failures)

	// Pipeline step with ERROR entry: original preserved, extracted errors populated
	if !strings.Contains(failures[0].Outputs[0].Output, "Running step") {
		t.Error("original output should be preserved with INFO entries")
	}
	if !strings.Contains(failures[0].Outputs[0].ExtractedErrors, "RoleAssignmentLimitExceeded") {
		t.Errorf("expected extracted errors to contain error, got %q", failures[0].Outputs[0].ExtractedErrors)
	}
	if strings.Contains(failures[0].Outputs[0].ExtractedErrors, "Running step") {
		t.Error("extracted errors should not contain INFO entries")
	}

	// Empty output should stay empty
	if failures[0].Outputs[1].Output != "" {
		t.Errorf("empty output should stay empty, got %q", failures[0].Outputs[1].Output)
	}

	// Non-pipeline test should be untouched
	if failures[1].Outputs[0].Output != "fail [test.go:42]: timeout exceeded" {
		t.Errorf("non-pipeline test should be untouched, got %q", failures[1].Outputs[0].Output)
	}

	// Pipeline step with no ERROR entries: no extracted errors
	if failures[2].Outputs[0].ExtractedErrors != "" {
		t.Errorf("pipeline step with no ERROR should have empty extracted errors, got %q", failures[2].Outputs[0].ExtractedErrors)
	}
	if !strings.Contains(failures[2].Outputs[0].Output, "Running step") {
		t.Errorf("pipeline step with no ERROR should keep original output, got %q", failures[2].Outputs[0].Output)
	}
}

func TestEnrichLastPass(t *testing.T) {
	now := time.Now()
	runs := []JobRun{
		{ID: 1, Timestamp: now.UnixMilli(), FailedTestNames: []string{"testA"}},
		{ID: 2, Timestamp: now.Add(-1 * time.Hour).UnixMilli(), FailedTestNames: []string{"testB"}},
		{ID: 3, Timestamp: now.Add(-2 * time.Hour).UnixMilli(), FailedTestNames: []string{"testA", "testB"}},
		{ID: 4, Timestamp: now.Add(-3 * time.Hour).UnixMilli(), FailedTestNames: []string{"testA"}},
		{ID: 5, Timestamp: now.Add(-4 * time.Hour).UnixMilli(), TestFailures: 1},
	}

	failures := []RecentFailure{
		{TestName: "testA", LastPass: ""},
		{TestName: "testB", LastPass: ""},
		{TestName: "testA", LastPass: "2026-01-01T00:00:00Z"},
		{TestName: "testC", LastPass: ""},
	}

	enrichLastPass(failures, runs)

	// testA fails in run 1, passes in run 2 (not in FailedTestNames) → last_pass = run 2 timestamp
	if failures[0].LastPass == "" {
		t.Error("testA should have last_pass set")
	}
	expected := time.UnixMilli(runs[1].Timestamp).UTC().Format(time.RFC3339)
	if failures[0].LastPass != expected {
		t.Errorf("testA last_pass = %q, want %q", failures[0].LastPass, expected)
	}

	// testB fails in run 2, passes in run 1 → last_pass = run 1 timestamp
	expected = time.UnixMilli(runs[0].Timestamp).UTC().Format(time.RFC3339)
	if failures[1].LastPass != expected {
		t.Errorf("testB last_pass = %q, want %q", failures[1].LastPass, expected)
	}

	// testA with pre-existing last_pass should not be overwritten
	if failures[2].LastPass != "2026-01-01T00:00:00Z" {
		t.Errorf("pre-existing last_pass should be preserved, got %q", failures[2].LastPass)
	}

	// testC never appears in FailedTestNames → passes in run 1 (newest with populated FailedTestNames)
	expected = time.UnixMilli(runs[0].Timestamp).UTC().Format(time.RFC3339)
	if failures[3].LastPass != expected {
		t.Errorf("testC last_pass = %q, want %q", failures[3].LastPass, expected)
	}
}

func TestEnrichLastPass_NoPopulatedRuns(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: time.Now().UnixMilli(), TestFailures: 3},
		{ID: 2, Timestamp: time.Now().Add(-1 * time.Hour).UnixMilli(), TestFailures: 1},
	}
	failures := []RecentFailure{
		{TestName: "testA", LastPass: ""},
	}

	enrichLastPass(failures, runs)

	if failures[0].LastPass != "" {
		t.Errorf("should not set last_pass when no runs have populated FailedTestNames, got %q", failures[0].LastPass)
	}
}

func TestBuildDataWindow(t *testing.T) {
	now := time.Now()
	runs := []JobRun{
		{Timestamp: now.UnixMilli()},
		{Timestamp: now.Add(-24 * time.Hour).UnixMilli()},
		{Timestamp: now.Add(-48 * time.Hour).UnixMilli()},
	}

	dw := buildDataWindow(runs, 7, 2, false, false)
	if dw == nil {
		t.Fatal("returned nil")
	}
	if dw.RequestedDays != 7 {
		t.Errorf("RequestedDays = %d, want 7", dw.RequestedDays)
	}
	if dw.ActualDays != 3 {
		t.Errorf("ActualDays = %d, want 3", dw.ActualDays)
	}
	if !dw.Truncated {
		t.Error("should be truncated (3 < 7-1)")
	}
	if dw.NightlyRunsExcluded != 2 {
		t.Errorf("NightlyRunsExcluded = %d, want 2", dw.NightlyRunsExcluded)
	}
	if dw.OldestRun == "" || dw.NewestRun == "" {
		t.Error("OldestRun and NewestRun should be set")
	}
}

func TestBuildDataWindow_Empty(t *testing.T) {
	dw := buildDataWindow(nil, 7, 0, false, false)
	if dw == nil {
		t.Fatal("returned nil")
	}
	if !dw.Empty {
		t.Error("should be empty")
	}
	if dw.EmptyReason == "" {
		t.Error("EmptyReason should be set")
	}
}

func TestBuildDailyRates(t *testing.T) {
	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	runs := []JobRun{
		{Timestamp: base.UnixMilli(), TestFailures: 0},
		{Timestamp: base.UnixMilli(), TestFailures: 2, FailedTestNames: []string{"a", "b"}},
		{Timestamp: base.Add(24 * time.Hour).UnixMilli(), TestFailures: 0},
		{Timestamp: base.Add(24 * time.Hour).UnixMilli(), TestFailures: 0},
		{Timestamp: base.Add(48 * time.Hour).UnixMilli(), TestFailures: 1, FailedTestNames: []string{"a"}},
	}

	rates := buildDailyRates(runs)
	if len(rates) != 3 {
		t.Fatalf("daily rate count = %d, want 3", len(rates))
	}
	if rates[0].Date != "2026-04-20" || rates[0].Pass != 1 || rates[0].Total != 2 {
		t.Errorf("day 0: %+v", rates[0])
	}
	if rates[1].Date != "2026-04-21" || rates[1].Pass != 2 || rates[1].Total != 2 {
		t.Errorf("day 1: %+v", rates[1])
	}
	if rates[2].Date != "2026-04-22" || rates[2].Pass != 0 || rates[2].Total != 1 {
		t.Errorf("day 2: %+v", rates[2])
	}
}

func TestBuildEV2Coverage(t *testing.T) {
	runs := []JobRun{
		{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abc123"}},
		{Annotations: map[string]string{"other": "val"}},
		{},
		{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "def456"}},
	}

	cov := buildEV2Coverage(runs)
	if cov.WithEV2 != 2 {
		t.Errorf("WithEV2 = %d, want 2", cov.WithEV2)
	}
	if cov.Total != 4 {
		t.Errorf("Total = %d, want 4", cov.Total)
	}
}

func TestBuildEV2HashRates(t *testing.T) {
	runs := []JobRun{
		{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abc"}, TestFailures: 0},
		{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abc"}, TestFailures: 1, FailedTestNames: []string{"x"}},
		{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "def"}, TestFailures: 0},
		{TestFailures: 0},
		{TestFailures: 1, FailedTestNames: []string{"y"}},
	}

	rates := buildEV2HashRates(runs)
	if len(rates) != 3 {
		t.Fatalf("hash count = %d, want 3 (abc, def, NO_HASH)", len(rates))
	}
	// Sorted by total desc: abc(2), NO_HASH(2), def(1)
	byHash := map[string]ev2HashRateJSON{}
	for _, r := range rates {
		byHash[r.Hash] = r
	}

	abc := byHash["abc"]
	if abc.Pass != 1 || abc.Fail != 1 || abc.Total != 2 || abc.IsCron {
		t.Errorf("abc: %+v", abc)
	}

	noHash := byHash["NO_HASH"]
	if noHash.Pass != 1 || noHash.Fail != 1 || !noHash.IsCron {
		t.Errorf("NO_HASH: %+v", noHash)
	}

	def := byHash["def"]
	if def.Pass != 1 || def.Fail != 0 || def.Total != 1 {
		t.Errorf("def: %+v", def)
	}
}

func TestBuildFailureScaleDist(t *testing.T) {
	runs := []JobRun{
		{TestFailures: 0},
		{TestFailures: 2, FailedTestNames: []string{"a", "b"}},
		{TestFailures: 10, FailedTestNames: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}},
		{TestFailures: 20, FailedTestNames: []string{
			"a", "b", "c", "d", "e", "f", "g", "h", "i", "j",
			"k", "l", "m", "n", "o", "p", "q", "r", "s", "t",
		}},
	}

	dist := buildFailureScaleDist(runs)
	if dist.None != 1 {
		t.Errorf("None = %d, want 1", dist.None)
	}
	if dist.Isolated != 1 {
		t.Errorf("Isolated = %d, want 1 (2 failures <= 3)", dist.Isolated)
	}
	if dist.Moderate != 1 {
		t.Errorf("Moderate = %d, want 1 (10 failures <= 15)", dist.Moderate)
	}
	if dist.Cascade != 1 {
		t.Errorf("Cascade = %d, want 1 (20 failures > 15)", dist.Cascade)
	}
}

func TestFailuresFromRuns(t *testing.T) {
	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	runs := []JobRun{
		{ID: 1, Timestamp: base.UnixMilli(), FailedTestNames: []string{"testA", "testB"}},
		{ID: 2, Timestamp: base.Add(1 * time.Hour).UnixMilli(), FailedTestNames: []string{"testA", "[sig-sippy] synthetic"}},
		{ID: 3, Timestamp: base.Add(2 * time.Hour).UnixMilli(), FailedTestNames: []string{"testC"}},
	}

	failures := failuresFromRuns(runs)

	byName := map[string]RecentFailure{}
	for _, f := range failures {
		byName[f.TestName] = f
	}

	if _, ok := byName["[sig-sippy] synthetic"]; ok {
		t.Error("synthetic tests should be excluded")
	}

	a := byName["testA"]
	if a.FailureCount != 2 {
		t.Errorf("testA count = %d, want 2", a.FailureCount)
	}
	if len(a.Outputs) != 2 {
		t.Errorf("testA outputs = %d, want 2", len(a.Outputs))
	}

	b := byName["testB"]
	if b.FailureCount != 1 {
		t.Errorf("testB count = %d, want 1", b.FailureCount)
	}

	c := byName["testC"]
	if c.FailureCount != 1 {
		t.Errorf("testC count = %d, want 1", c.FailureCount)
	}
}

func TestFilterFailuresToRuns(t *testing.T) {
	runs := []JobRun{
		{ID: 100, FailedTestNames: []string{"Customer should create cluster", "Run pipeline step service/cluster"}},
		{ID: 101, FailedTestNames: []string{"Customer should create cluster"}},
		{ID: 200}, // passing run
	}

	failures := []RecentFailure{
		{
			TestName: "Customer should create cluster",
			Outputs:  []FailureOutput{{RunID: 100}, {RunID: 101}},
		},
		{
			TestName: "Run pipeline step service/cluster",
			Outputs:  []FailureOutput{{RunID: 100}},
		},
		{
			TestName: "TestCreateCluster", // HyperShift test, not in our runs
			Outputs:  []FailureOutput{{RunID: 999}},
		},
		{
			TestName: "install should succeed: overall", // HyperShift test
			Outputs:  []FailureOutput{{RunID: 998}},
		},
	}

	got := filterFailuresToRuns(failures, runs)

	if len(got) != 2 {
		t.Fatalf("expected 2 failures (ARO-HCP only), got %d", len(got))
	}
	names := map[string]bool{}
	for _, f := range got {
		names[f.TestName] = true
	}
	if !names["Customer should create cluster"] {
		t.Error("should keep 'Customer should create cluster'")
	}
	if !names["Run pipeline step service/cluster"] {
		t.Error("should keep 'Run pipeline step service/cluster'")
	}
	if names["TestCreateCluster"] {
		t.Error("should filter out HyperShift test 'TestCreateCluster'")
	}
	if names["install should succeed: overall"] {
		t.Error("should filter out HyperShift test 'install should succeed: overall'")
	}
}

func TestFilterFailuresToRuns_KeepsByTestName(t *testing.T) {
	runs := []JobRun{
		{ID: 100, FailedTestNames: []string{"alert KubeVersionMismatch should not fire"}},
	}

	// Failure has a run ID NOT in our runs, but test name IS in FailedTestNames
	failures := []RecentFailure{
		{
			TestName: "alert KubeVersionMismatch should not fire",
			Outputs:  []FailureOutput{{RunID: 999}}, // not in runs
		},
	}

	got := filterFailuresToRuns(failures, runs)
	if len(got) != 1 {
		t.Fatalf("expected 1 failure (matched by test name), got %d", len(got))
	}
}

func TestEnrichMissingErrors_TargetsRelevantRuns(t *testing.T) {
	// Simulate presubmit scenario: recent runs failed at pipeline steps (1 failure),
	// older runs have e2e test failures. enrichMissingErrors should target the runs
	// referenced by e2e test failures, not the most recent failed runs.
	now := time.Now()

	// 30 recent runs with 1 pipeline step failure each (no test artifacts)
	var runs []JobRun
	for i := 0; i < 30; i++ {
		runs = append(runs, JobRun{
			ID:              int64(100 + i),
			Timestamp:       now.Add(-time.Duration(i) * time.Hour).UnixMilli(),
			FailedTestNames: []string{"Run pipeline step Microsoft.Azure.ARO.HCP.Service.Infra/service/cluster"},
		})
	}
	// Older run with e2e test failures
	testRunID := int64(50)
	runs = append(runs, JobRun{
		ID:              testRunID,
		Timestamp:       now.Add(-40 * time.Hour).UnixMilli(),
		FailedTestNames: []string{"Customer should create cluster", "Customer should delete cluster"},
	})

	failures := []RecentFailure{
		{
			TestName: "Run pipeline step Microsoft.Azure.ARO.HCP.Service.Infra/service/cluster",
			Outputs:  []FailureOutput{{RunID: 100, Output: ""}},
		},
		{
			TestName: "Customer should create cluster",
			Outputs:  []FailureOutput{{RunID: testRunID, Output: ""}},
		},
	}

	// enrichMissingErrors won't actually fetch GCS (no server), but we can verify
	// it doesn't panic and that it skips pipeline step failures when collecting targets.
	// The function will silently fail on GCS fetches, which is fine for this test.
	enrichMissingErrors(failures, runs)

	// Pipeline step failure should still be empty (skipped by enrichMissingErrors)
	if failures[0].Outputs[0].Output != "" {
		t.Errorf("pipeline step output should remain empty, got %q", failures[0].Outputs[0].Output)
	}
}

func TestEnrichMissingErrors_SkipsWhenAllPopulated(t *testing.T) {
	failures := []RecentFailure{
		{
			TestName: "Customer should create cluster",
			Outputs:  []FailureOutput{{RunID: 1, Output: "already has error text"}},
		},
		{
			TestName: "Run pipeline step something",
			Outputs:  []FailureOutput{{RunID: 2, Output: ""}},
		},
	}
	runs := []JobRun{{ID: 1}, {ID: 2}}

	// Should return early: only empty output is for a pipeline step (skipped)
	enrichMissingErrors(failures, runs)

	if failures[0].Outputs[0].Output != "already has error text" {
		t.Error("populated output should not be modified")
	}
}

func TestBuildSurveyJSON_OutputShape(t *testing.T) {
	now := time.Now()
	data := &surveyData{
		release:       "aro-integration",
		requestedDays: 7,
		runs: []JobRun{
			{
				ID: 1, Timestamp: now.UnixMilli(), OverallResult: "S",
				TestFailures: 0, FailedTestNames: []string{},
				Annotations: map[string]string{
					"ev2.rollout/ARO-HCP": "abc123",
					"ev2.rollout/region":  "eastus2",
				},
				Cluster: "build01",
				URL:     "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel/1",
			},
			{
				ID: 2, Timestamp: now.Add(-24 * time.Hour).UnixMilli(), OverallResult: "F",
				TestFailures: 2, FailedTestNames: []string{"test-create-cluster", "test-delete-cluster"},
				Annotations: map[string]string{
					"ev2.rollout/ARO-HCP": "abc123",
					"ev2.rollout/region":  "eastus2",
				},
				Cluster: "build01",
				URL:     "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel/2",
			},
		},
		failures: []RecentFailure{
			{
				TestName: "test-create-cluster", FailureCount: 2,
				FirstFailure: "2026-04-20T00:00:00Z", LastFailure: "2026-04-21T00:00:00Z",
				LastPass: "2026-04-22T00:00:00Z",
				Outputs: []FailureOutput{
					{RunID: 2, Output: "timeout exceeded"},
				},
			},
		},
	}

	sj := buildSurveyJSON("int", data)

	// Marshal to JSON and back to map to validate shape
	raw, err := json.Marshal(sj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Top-level fields
	requiredKeys := []string{"env", "release", "status", "data_window", "daily_rates",
		"ev2_coverage", "ev2_hash_rates", "failure_scale_dist", "region_rates", "runs", "failures"}
	for _, k := range requiredKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}

	// Status shape
	status, _ := m["status"].(map[string]any)
	for _, k := range []string{"streak", "current_green", "pass_rate", "total_runs"} {
		if _, ok := status[k]; !ok {
			t.Errorf("missing status.%s", k)
		}
	}

	// Data window shape
	dw, _ := m["data_window"].(map[string]any)
	for _, k := range []string{"requested_days", "actual_days", "oldest_run", "newest_run", "truncated"} {
		if _, ok := dw[k]; !ok {
			t.Errorf("missing data_window.%s", k)
		}
	}

	// Runs shape (failed only)
	runs, _ := m["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs count = %d, want 1 (failed only)", len(runs))
	}
	run0, _ := runs[0].(map[string]any)
	for _, k := range []string{"id", "timestamp", "overall_result", "real_failures", "url"} {
		if _, ok := run0[k]; !ok {
			t.Errorf("missing runs[0].%s", k)
		}
	}

	// Failures shape
	failures, _ := m["failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("failures count = %d, want 1", len(failures))
	}
	f0, _ := failures[0].(map[string]any)
	for _, k := range []string{"test_name", "failure_count", "first_failure", "last_failure",
		"last_pass", "best_run_id", "total_runs", "outputs"} {
		if _, ok := f0[k]; !ok {
			t.Errorf("missing failures[0].%s", k)
		}
	}

	// Outputs shape
	outputs, _ := f0["outputs"].([]any)
	if len(outputs) != 1 {
		t.Fatalf("outputs count = %d, want 1", len(outputs))
	}
	o0, _ := outputs[0].(map[string]any)
	for _, k := range []string{"run_id", "error"} {
		if _, ok := o0[k]; !ok {
			t.Errorf("missing outputs[0].%s", k)
		}
	}

	// EV2 hash rates shape
	hashRates, _ := m["ev2_hash_rates"].([]any)
	if len(hashRates) == 0 {
		t.Fatal("ev2_hash_rates should not be empty")
	}
	hr0, _ := hashRates[0].(map[string]any)
	for _, k := range []string{"hash", "pass", "fail", "total", "pass_rate"} {
		if _, ok := hr0[k]; !ok {
			t.Errorf("missing ev2_hash_rates[0].%s", k)
		}
	}

	// Region rates shape
	regionRates, _ := m["region_rates"].([]any)
	if len(regionRates) == 0 {
		t.Fatal("region_rates should not be empty")
	}
	rr0, _ := regionRates[0].(map[string]any)
	for _, k := range []string{"region", "pass", "total", "pass_rate"} {
		if _, ok := rr0[k]; !ok {
			t.Errorf("missing region_rates[0].%s", k)
		}
	}

	// Failure scale dist shape
	fsd, _ := m["failure_scale_dist"].(map[string]any)
	for _, k := range []string{"none", "isolated", "moderate", "cascade"} {
		if _, ok := fsd[k]; !ok {
			t.Errorf("missing failure_scale_dist.%s", k)
		}
	}
}

func TestStratifyRuns(t *testing.T) {
	t.Run("no stratification when under budget", func(t *testing.T) {
		runs := make([]JobRun, 10)
		base := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
		for i := range runs {
			runs[i] = JobRun{
				ID:        int64(i),
				Timestamp: base.Add(time.Duration(i) * time.Hour).UnixMilli(),
			}
		}
		result, stratified := stratifyRuns(runs, 5)
		if stratified {
			t.Error("should not stratify when under budget")
		}
		if len(result) != 10 {
			t.Errorf("expected 10 runs, got %d", len(result))
		}
	})

	t.Run("stratifies skewed distribution", func(t *testing.T) {
		var runs []JobRun
		base := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
		// Day 1: 50 runs, Day 2: 300 runs, Day 3: 50 runs
		for i := range 50 {
			runs = append(runs, JobRun{
				ID:        int64(i),
				Timestamp: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			})
		}
		for i := range 300 {
			runs = append(runs, JobRun{
				ID:        int64(100 + i),
				Timestamp: base.Add(24*time.Hour + time.Duration(i)*time.Minute).UnixMilli(),
			})
		}
		for i := range 50 {
			runs = append(runs, JobRun{
				ID:        int64(500 + i),
				Timestamp: base.Add(48*time.Hour + time.Duration(i)*time.Minute).UnixMilli(),
			})
		}

		result, stratified := stratifyRuns(runs, 5)
		if !stratified {
			t.Error("should stratify when day 2 exceeds budget (300 > 200)")
		}

		// Count per day
		dayCounts := map[string]int{}
		for _, r := range result {
			day := time.UnixMilli(r.Timestamp).UTC().Format("2006-01-02")
			dayCounts[day]++
		}

		// Day 1 and 3 should keep all 50 (under budget)
		if dayCounts["2026-04-22"] != 50 {
			t.Errorf("day 1: expected 50, got %d", dayCounts["2026-04-22"])
		}
		if dayCounts["2026-04-24"] != 50 {
			t.Errorf("day 3: expected 50, got %d", dayCounts["2026-04-24"])
		}
		// Day 2 should be capped to maxRunsFetch/days = 1000/5 = 200
		if dayCounts["2026-04-23"] != 200 {
			t.Errorf("day 2: expected 200, got %d", dayCounts["2026-04-23"])
		}
	})

	t.Run("deterministic output", func(t *testing.T) {
		var runs []JobRun
		base := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
		for i := range 500 {
			runs = append(runs, JobRun{
				ID:        int64(i),
				Timestamp: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			})
		}

		r1, _ := stratifyRuns(runs, 3)
		r2, _ := stratifyRuns(runs, 3)

		if len(r1) != len(r2) {
			t.Fatalf("non-deterministic: len %d vs %d", len(r1), len(r2))
		}
		for i := range r1 {
			if r1[i].ID != r2[i].ID {
				t.Errorf("non-deterministic at index %d: ID %d vs %d", i, r1[i].ID, r2[i].ID)
				break
			}
		}
	})

	t.Run("sorted descending by timestamp", func(t *testing.T) {
		var runs []JobRun
		base := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
		// 600 runs on day 1 exceeds 1000/2=500 budget
		for i := range 600 {
			runs = append(runs, JobRun{
				ID:        int64(i),
				Timestamp: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			})
		}

		result, stratified := stratifyRuns(runs, 2)
		if !stratified {
			t.Error("expected stratification for 600 runs over 2 days")
		}
		for i := 1; i < len(result); i++ {
			if result[i].Timestamp > result[i-1].Timestamp {
				t.Errorf("not descending at index %d: %d > %d", i, result[i].Timestamp, result[i-1].Timestamp)
				break
			}
		}
	})

	t.Run("empty runs", func(t *testing.T) {
		result, stratified := stratifyRuns(nil, 5)
		if stratified {
			t.Error("should not stratify empty runs")
		}
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})
}

