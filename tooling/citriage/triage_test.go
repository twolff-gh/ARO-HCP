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
