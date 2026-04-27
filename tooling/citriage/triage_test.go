package main

import (
	"testing"
)

func TestBuildRunContext(t *testing.T) {
	cases := []struct {
		name       string
		summary    RunSummary
		wantEnv    string
		wantPresub bool
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

func TestParseJUnitForTriage(t *testing.T) {
	input := []byte(`<testsuites>
		<testsuite name="e2e" tests="4" failures="2">
			<testcase name="test-create-cluster" time="600.0">
				<failure message="timeout exceeded">details here</failure>
			</testcase>
			<testcase name="test-list-clusters" time="5.0"></testcase>
			<testcase name="test-delete-cluster" time="120.0">
				<error message="resource not found">stack trace</error>
			</testcase>
			<testcase name="test-get-cluster" time="2.0">
				<skipped/>
			</testcase>
		</testsuite>
	</testsuites>`)

	entries := parseJUnitForTriage(input)
	if len(entries) != 2 {
		t.Fatalf("entry count = %d, want 2 (failures only)", len(entries))
	}

	if entries[0].name != "test-create-cluster" || entries[0].err != "timeout exceeded" {
		t.Errorf("entry 0: name=%q err=%q", entries[0].name, entries[0].err)
	}
	if entries[1].name != "test-delete-cluster" || entries[1].err != "resource not found" {
		t.Errorf("entry 1: name=%q err=%q", entries[1].name, entries[1].err)
	}
}

func TestParseJUnitForTriage_Invalid(t *testing.T) {
	if entries := parseJUnitForTriage([]byte("not xml")); entries != nil {
		t.Errorf("expected nil for invalid XML, got %d entries", len(entries))
	}
}
