package main

import (
	"strings"
	"testing"
)

func TestNormalizeContext(t *testing.T) {
	lines := []string{
		`failed to create HCP cluster in resource group rg-update-nodes-c8bwjt`,
		`timeout exceeded for cluster np-autoscale-cluster in rg np-autoscaling-5ddsnm`,
		`run 2045053983071932416 at 2026-04-17T08:17:55Z`,
	}
	norm := normalizeContext("junit", lines)

	if norm == "junit|"+lines[0]+"\n"+lines[1]+"\n"+lines[2] {
		t.Error("normalizeContext should have replaced dynamic tokens")
	}
	if strings.Contains(norm, "2045053983071932416") {
		t.Error("run ID should be normalized out")
	}
	if strings.Contains(norm, "2026-04-17T08:17:55Z") {
		t.Error("timestamp should be normalized out")
	}
}

func TestSearchJobFilter(t *testing.T) {
	tests := []struct {
		env, job, want string
	}{
		{"", "", "Azure-ARO-HCP"},
		{"int", "", "periodic-integration-e2e-parallel"},
		{"stg", "", "periodic-stage-e2e-parallel"},
		{"prod", "", "periodic-prod-e2e-parallel"},
		{"dev", "", "e2e-parallel"},
		{"int", "custom-job", "custom-job"},
		{"", "custom-job", "custom-job"},
	}
	for _, tt := range tests {
		if got := searchJobFilter(tt.env, tt.job); got != tt.want {
			t.Errorf("searchJobFilter(%q, %q) = %q, want %q", tt.env, tt.job, got, tt.want)
		}
	}
}

func TestMatchSuffixedToken(t *testing.T) {
	tests := []struct {
		input     string
		wantSkip  bool
		desc      string
	}{
		{"cilium-pzg6jc rest", true, "random suffix with digit"},
		{"cluster-kj9xq5 rest", true, "random suffix with digit"},
		{"registry-2wl4g4 rest", true, "random suffix starting with digit"},
		{"cilium-cluster rest", false, "real compound identifier (no digit in suffix)"},
		{"labels-taints rest", false, "real compound identifier (no digit in suffix)"},
		{"cluster-zstream rest", false, "real compound identifier (no digit in suffix)"},
		{"version-upgrade rest", false, "real compound identifier (no digit in suffix)"},
		{"shoebox-cluster rest", false, "real compound identifier (no digit in suffix)"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			_, gotSkip := matchSuffixedToken(tt.input, 0)
			if gotSkip != tt.wantSkip {
				t.Errorf("matchSuffixedToken(%q, 0) skip=%v, want %v", tt.input, gotSkip, tt.wantSkip)
			}
		})
	}
}

func TestSplitByType(t *testing.T) {
	matches := []searchMatch{
		{file: "junit", isIssue: false},
		{file: "issue", isIssue: true},
		{file: "build-log", isIssue: false},
		{file: "issue", isIssue: true},
	}
	tests, issues := splitByType(matches)
	if len(tests) != 2 {
		t.Errorf("got %d test matches, want 2", len(tests))
	}
	if len(issues) != 2 {
		t.Errorf("got %d issue matches, want 2", len(issues))
	}
}
