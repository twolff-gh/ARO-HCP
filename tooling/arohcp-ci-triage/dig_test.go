package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestStepContainer(t *testing.T) {
	tests := []struct {
		job      string
		wantStep string
		wantCtr  string
	}{
		{"periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel", "integration-e2e-parallel", "aro-hcp-test-persistent"},
		{"periodic-ci-Azure-ARO-HCP-main-periodic-stage-e2e-parallel", "stage-e2e-parallel", "aro-hcp-test-persistent"},
		{"periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel", "prod-e2e-parallel", "aro-hcp-test-persistent"},
		{"pull-ci-Azure-ARO-HCP-main-e2e-parallel", "e2e-parallel", "aro-hcp-test-local-run"},
		{"periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel-ocp-nightly", "prod-e2e-parallel", "aro-hcp-test-persistent"},
	}
	for _, tt := range tests {
		step, ctr := stepContainer(tt.job)
		if step != tt.wantStep || ctr != tt.wantCtr {
			t.Errorf("stepContainer(%q) = (%q, %q), want (%q, %q)", tt.job, step, ctr, tt.wantStep, tt.wantCtr)
		}
	}
}

func TestIsPresubmitJob(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"pull-ci-Azure-ARO-HCP-main-e2e-parallel", true},
		{"periodic-ci-Azure-ARO-HCP-main-periodic-integration-e2e-parallel", false},
		{"periodic-ci-Azure-ARO-HCP-main-periodic-prod-e2e-parallel", false},
	}
	for _, tt := range tests {
		if got := isPresubmitJob(tt.name); got != tt.want {
			t.Errorf("isPresubmitJob(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func newTestDigContext(buf *bytes.Buffer) *digContext {
	return &digContext{out: buf, summary: &RunSummary{}}
}

func TestDigTestsFromJUnit(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="suite" tests="3" failures="1">
  <testcase name="test-pass" time="1.5"/>
  <testcase name="test-fail" time="2.3">
    <failure message="something broke">detailed error</failure>
  </testcase>
  <testcase name="test-skip" time="0">
    <skipped/>
  </testcase>
</testsuite>`

	var buf bytes.Buffer
	d := newTestDigContext(&buf)
	err := d.testsFromJUnit([]byte(xmlData), "test.xml")
	if err != nil {
		t.Fatal(err)
	}

	var env struct {
		Data testsJSON `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}

	if env.Data.Failed != 1 || env.Data.Passed != 1 || env.Data.Skipped != 1 {
		t.Errorf("got %d failed, %d passed, %d skipped; want 1, 1, 1",
			env.Data.Failed, env.Data.Passed, env.Data.Skipped)
	}
	for _, test := range env.Data.Tests {
		if test.Name == "test-fail" {
			if test.Error != "something broke" {
				t.Errorf("error = %q, want %q", test.Error, "something broke")
			}
			return
		}
	}
	t.Error("test-fail not found in results")
}

func TestDigTestsFromJUnitTestSuites(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="s1" tests="2" failures="1">
    <testcase name="pass1" time="0.5"/>
    <testcase name="fail1" time="1.0">
      <error message="err msg">body</error>
    </testcase>
  </testsuite>
  <testsuite name="s2" tests="1" failures="0">
    <testcase name="pass2" time="0.3"/>
  </testsuite>
</testsuites>`

	var buf bytes.Buffer
	d := newTestDigContext(&buf)
	err := d.testsFromJUnit([]byte(xmlData), "test.xml")
	if err != nil {
		t.Fatal(err)
	}

	var env struct {
		Data testsJSON `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}

	if env.Data.Failed != 1 || env.Data.Passed != 2 || env.Data.Skipped != 0 {
		t.Errorf("got %d failed, %d passed, %d skipped; want 1, 2, 0",
			env.Data.Failed, env.Data.Passed, env.Data.Skipped)
	}
}

func TestDetectPoolContention(t *testing.T) {
	tests := []struct {
		desc    string
		content string
		want    int
	}{
		{
			desc:    "healthy pool",
			content: "- current:\n  state: free\nhistory:\n  state: free\n",
			want:    0,
		},
		{
			desc:    "contention detected",
			content: "- current:\n  state: assigned\n  state: busy\nhistory:\n",
			want:    2,
		},
		{
			desc:    "empty content",
			content: "",
			want:    0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			warnings := detectPoolContentionFromString(tt.content)
			if len(warnings) != tt.want {
				t.Errorf("detectPoolContentionFromString() returned %d warnings, want %d: %v", len(warnings), tt.want, warnings)
			}
		})
	}
}

func TestGcsBase(t *testing.T) {
	tests := []struct {
		url, want string
	}{
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/logs/job/123", "logs/job/123/"},
		{"https://prow.ci.openshift.org/view/gs/test-platform-results/logs/job/123/", "logs/job/123/"},
		{"https://other.example.com/path", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := gcsBase(tt.url); got != tt.want {
			t.Errorf("gcsBase(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestSanitizeTest(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Customer should create cluster", "Customer_should_create_cluster"},
		{"no-spaces", "no-spaces"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeTest(tt.in); got != tt.want {
			t.Errorf("sanitizeTest(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDetectOOM(t *testing.T) {
	oomData := `{"pod":{"status":{"containerStatuses":[{"name":"test","state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}}`
	noOOMData := `{"pod":{"status":{"containerStatuses":[{"name":"test","state":{"terminated":{"reason":"Completed","exitCode":0}}}]}}}`
	invalidData := `not json`

	if s := extractPodinfoSummary([]byte(oomData)); s == nil || !s.OOMDetected {
		t.Error("expected OOM detected")
	}
	if s := extractPodinfoSummary([]byte(noOOMData)); s != nil && s.OOMDetected {
		t.Error("expected no OOM")
	}
	if s := extractPodinfoSummary([]byte(invalidData)); s != nil {
		t.Error("expected nil for invalid data")
	}
}
