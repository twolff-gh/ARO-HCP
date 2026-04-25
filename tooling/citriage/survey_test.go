package main

import (
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

func TestStripGoPointers(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"no pointers here", "no pointers here"},
		{"value at 0x00c0001a4b80 is wrong", "value at  is wrong"},
		{"<*v1.Pod | 0x00c0001a4b80>: had error", "had error"},
		{"short 0x1f is fine", "short 0x1f is fine"},
		{"multiple 0xdeadbeef01 and 0x00c000123456", "multiple  and "},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripGoPointers(tt.in); got != tt.want {
			t.Errorf("stripGoPointers(%q) = %q, want %q", tt.in, got, tt.want)
		}
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
	got := filterNightlyRuns(runs)
	if len(got) != 2 {
		t.Fatalf("filterNightlyRuns returned %d runs, want 2", len(got))
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
		{"long hash truncated", JobRun{Annotations: map[string]string{"ev2.rollout/ARO-HCP": "abcdef1234567890abcd"}}, "abcdef123456"},
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

func TestNeedsErrorEnrichment(t *testing.T) {
	tests := []struct {
		desc     string
		failures []RecentFailure
		want     bool
	}{
		{
			desc:     "all have outputs",
			failures: []RecentFailure{{Outputs: []FailureOutput{{Output: "error text"}}}},
			want:     false,
		},
		{
			desc:     "none have outputs",
			failures: []RecentFailure{{Outputs: []FailureOutput{{Output: ""}}}},
			want:     true,
		},
		{
			desc: "mixed — some with, some without",
			failures: []RecentFailure{
				{Outputs: []FailureOutput{{Output: "error text"}}},
				{Outputs: []FailureOutput{{Output: ""}}},
			},
			want: true,
		},
		{
			desc:     "empty list",
			failures: []RecentFailure{},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := needsErrorEnrichment(tt.failures); got != tt.want {
				t.Errorf("needsErrorEnrichment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripTimestamps(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"no timestamps here", "no timestamps here"},
		{
			"time=2026-04-17T23:26:22.973Z level=INFO msg=\"Running step.\"",
			"time= level=INFO msg=\"Running step.\"",
		},
		{
			"at 2026-04-17T23:26:22Z it broke",
			"at  it broke",
		},
		{
			"first 2026-04-17T23:26:22.973Z then 2026-04-18T00:04:45.447Z",
			"first  then ",
		},
		{
			"offset 2026-04-17T23:26:22.973+05:30 timezone",
			"offset  timezone",
		},
		{"", ""},
		{"2026-04-17 is not a full timestamp", "2026-04-17 is not a full timestamp"},
	}
	for _, tt := range tests {
		if got := stripTimestamps(tt.in); got != tt.want {
			t.Errorf("stripTimestamps(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGroupErrorOutputsFull_TimestampNormalization(t *testing.T) {
	f := RecentFailure{
		TestName: "deploy-mce",
		Outputs: []FailureOutput{
			{RunID: 1, Output: "time=2026-04-17T23:26:22.973Z level=INFO msg=\"deploy-mce\" step=deploy-mce"},
			{RunID: 2, Output: "time=2026-04-18T00:04:45.447Z level=INFO msg=\"deploy-mce\" step=deploy-mce"},
			{RunID: 3, Output: "time=2026-04-19T22:18:50.229Z level=INFO msg=\"deploy-mce\" step=deploy-mce"},
		},
	}
	groups := groupErrorOutputsFull(f)
	if len(groups) != 1 {
		t.Errorf("expected 1 error group after timestamp normalization, got %d", len(groups))
		for i, g := range groups {
			t.Logf("  group %d (count=%d): %s", i, g.Count, g.Error[:min(100, len(g.Error))])
		}
		return
	}
	if groups[0].Count != 3 {
		t.Errorf("expected count=3, got %d", groups[0].Count)
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

func TestNormalizeForSimilarity(t *testing.T) {
	tests := []struct {
		desc string
		in   string
		// We check that certain substrings are present/absent after normalization
		wantContains    []string
		wantNotContains []string
	}{
		{
			"strips source location",
			"fail [test.go:112]: some error",
			[]string{"some error"},
			[]string{"test.go"},
		},
		{
			"strips timestamps",
			"time=2026-04-19T17:50:14.082Z level=INFO msg=foo",
			[]string{"level=info", "msg=foo"},
			[]string{"2026-04-19"},
		},
		{
			"strips UUIDs",
			"operation 37230de7-e1d1-4a57-b4b5-81ff612cb3c2 failed",
			[]string{"operation", "failed"},
			[]string{"37230de7"},
		},
		{
			"strips Azure URLs",
			"GET https://management.azure.com/subscriptions/xxx/providers/foo failed",
			[]string{"get", "failed"},
			[]string{"management.azure.com"},
		},
		{
			"strips numeric values in quotes",
			"timeout '45.000000' minutes exceeded",
			[]string{"timeout", "minutes", "exceeded"},
			[]string{"45.000000"},
		},
		{
			"lowercases",
			"CreateHCPClusterFromParam ERROR",
			[]string{"createhcpclusterfromparam", "error"},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := normalizeForSimilarity(tt.in)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("normalizeForSimilarity() = %q, want to contain %q", got, want)
				}
			}
			for _, notWant := range tt.wantNotContains {
				if strings.Contains(got, notWant) {
					t.Errorf("normalizeForSimilarity() = %q, should NOT contain %q", got, notWant)
				}
			}
		})
	}
}

func TestNormalizedErrorInFailureJSON(t *testing.T) {
	f := RecentFailure{
		TestName:     "TestA",
		FailureCount: 3,
		FirstFailure: "2026-04-20T00:00:00Z",
		LastFailure:  "2026-04-22T00:00:00Z",
		Outputs: []FailureOutput{
			{RunID: 1, Output: "fail [idms.go:112]: Unexpected error: timeout '45.000000' minutes exceeded during CreateHCPClusterFromParam for cluster foo in resource group rg-foo, error: context deadline exceeded"},
			{RunID: 2, Output: "fail [idms.go:112]: Unexpected error: timeout '45.000000' minutes exceeded during CreateHCPClusterFromParam for cluster bar in resource group rg-bar, error: context deadline exceeded"},
			{RunID: 3, Output: "fail [idms.go:112]: tls: x509: certificate is valid for api.x, not api.y"},
		},
	}
	ranked := []rankedFailure{{failure: f, regularHits: 3, bestRunID: 1}}
	runMap := map[int64]JobRun{
		1: {ID: 1, Timestamp: 1000},
		2: {ID: 2, Timestamp: 2000},
		3: {ID: 3, Timestamp: 3000},
	}
	windowStart := time.UnixMilli(500).UTC()
	result := buildFailuresJSON(ranked, runMap, 3, nil, "", windowStart)

	if len(result) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(result))
	}
	ne := result[0].NormalizedError
	if ne == "" {
		t.Fatal("NormalizedError should not be empty")
	}
	if strings.Contains(ne, "idms.go") {
		t.Error("NormalizedError should not contain source file")
	}
	if strings.Contains(ne, "45.000000") {
		t.Error("NormalizedError should not contain raw numeric values")
	}
	if !strings.Contains(ne, "timeout") {
		t.Error("NormalizedError should contain 'timeout'")
	}
	if !strings.Contains(ne, "createhcpclusterfromparam") {
		t.Error("NormalizedError should contain lowercased function name")
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
			"returns empty for non-structured-log output",
			"some random error text without templatize format",
			"",
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

	// Pipeline step with ERROR entry: should be extracted
	if !strings.Contains(failures[0].Outputs[0].Output, "RoleAssignmentLimitExceeded") {
		t.Errorf("expected extracted error, got %q", failures[0].Outputs[0].Output)
	}
	if strings.Contains(failures[0].Outputs[0].Output, "Running step") {
		t.Error("extracted output should not contain INFO entries")
	}

	// Empty output should stay empty
	if failures[0].Outputs[1].Output != "" {
		t.Errorf("empty output should stay empty, got %q", failures[0].Outputs[1].Output)
	}

	// Non-pipeline test should be untouched
	if failures[1].Outputs[0].Output != "fail [test.go:42]: timeout exceeded" {
		t.Errorf("non-pipeline test should be untouched, got %q", failures[1].Outputs[0].Output)
	}

	// Pipeline step with no ERROR entries: should keep original
	if !strings.Contains(failures[2].Outputs[0].Output, "Running step") {
		t.Errorf("pipeline step with no ERROR should keep original, got %q", failures[2].Outputs[0].Output)
	}
}

