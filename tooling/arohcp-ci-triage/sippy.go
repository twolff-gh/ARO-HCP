package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sippyBase       = "https://sippy.dptools.openshift.org"
	sippyHTTPTimeout = 30 * time.Second

	ev2HashAnnotation   = "ev2.rollout/ARO-HCP"
	ev2RegionAnnotation = "ev2.rollout/region"

	syntheticTestPrefix  = "[sig-sippy]"
	syntheticTestTimeout = "Job run should complete before timeout"
)

// envRelease maps environment shorthand to Sippy release name.
var envRelease = map[string]string{
	"int":  "aro-integration",
	"stg":  "aro-stage",
	"prod": "aro-production",
	"dev":  "Presubmits",
}

// defaultJobFilter returns the Sippy job name filter for an environment.
func defaultJobFilter(env string) string {
	switch env {
	case "int":
		return "periodic-integration-e2e-parallel"
	case "stg":
		return "periodic-stage-e2e-parallel"
	case "prod":
		return "periodic-prod-e2e-parallel"
	case "dev":
		return "e2e-parallel"
	default:
		return "Azure-ARO-HCP"
	}
}

// sippy is a client for the Sippy fleet health API, providing access to
// run listings, test failure data, and run summaries.
type sippy struct {
	c *http.Client
}

func newSippy() *sippy {
	return &sippy{c: &http.Client{Timeout: sippyHTTPTimeout}}
}

func (s *sippy) get(path string, params url.Values) ([]byte, error) {
	reqURL := sippyBase + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}
	resp, err := s.c.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("sippy %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sippy %s: %d %s", path, resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

// JobRun is a single CI job execution from the Sippy fleet API.
type JobRun struct {
	ID              int64             `json:"id"`
	Job             string            `json:"job"`
	OverallResult   string            `json:"overall_result"`
	TestFailures    int               `json:"test_failures"`
	FailedTestNames []string          `json:"failed_test_names"`
	Timestamp       int64             `json:"timestamp"`
	Annotations     map[string]string `json:"annotations"`
	Cluster         string            `json:"cluster"`
	URL             string            `json:"url"`
	InfraFailure    bool              `json:"infrastructure_failure"`
	PullRequestSHA  string            `json:"pull_request_sha"`
}

// FailureOutput pairs a run ID with the test's error output for that run.
type FailureOutput struct {
	RunID           int64  `json:"prow_job_run_id"`
	Output          string `json:"output"`
	ExtractedErrors string `json:"-"`
}

// RecentFailure aggregates a test's failures across recent runs with per-run output.
type RecentFailure struct {
	TestName     string          `json:"test_name"`
	FailureCount int             `json:"failure_count"`
	FirstFailure string          `json:"first_failure"`
	LastFailure  string          `json:"last_failure"`
	LastPass     string          `json:"last_pass"`
	Outputs      []FailureOutput `json:"outputs"`
}

// RunSummary holds metadata and test counts for a single CI run.
type RunSummary struct {
	ID               int64             `json:"id"`
	Name             string            `json:"name"`
	Release          string            `json:"release"`
	URL              string            `json:"url"`
	StartTime        string            `json:"startTime"`
	DurationSeconds  int               `json:"durationSeconds"`
	OverallResult    string            `json:"overallResult"`
	TestCount        int               `json:"testCount"`
	TestFailureCount int               `json:"testFailureCount"`
	TestFailures     map[string]string `json:"testFailures"`
}

func (s *sippy) listRuns(release string, limit int, jobFilter string) ([]JobRun, error) {
	filterVal, _ := json.Marshal(jobFilter)
	filter := fmt.Sprintf(`{"items":[{"columnField":"job","operatorValue":"contains","value":%s}],"linkOperator":"and"}`, filterVal)
	data, err := s.get("/api/jobs/runs", url.Values{
		"release": {release}, "limit": {fmt.Sprintf("%d", limit)}, "filter": {filter},
	})
	if err != nil {
		return nil, err
	}
	var resp struct{ Rows []JobRun `json:"rows"` }
	return resp.Rows, json.Unmarshal(data, &resp)
}

func (s *sippy) recentFailures(release, period string) ([]RecentFailure, error) {
	data, err := s.get("/api/tests/recent_failures", url.Values{
		"release": {release}, "period": {period}, "includeOutputs": {"true"},
	})
	if err != nil {
		return nil, err
	}
	var resp struct{ Rows []RecentFailure `json:"rows"` }
	return resp.Rows, json.Unmarshal(data, &resp)
}

func extractPullNumber(prowURL string) int {
	_, rest, found := strings.Cut(prowURL, "/pull/")
	if !found {
		return 0
	}
	_, rest, found = strings.Cut(rest, "/")
	if !found {
		return 0
	}
	end := strings.Index(rest, "/")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

func (s *sippy) runSummary(runID string) (*RunSummary, error) {
	data, err := s.get("/api/job/run/summary", url.Values{"prow_job_run_id": {runID}})
	if err != nil {
		return nil, err
	}
	var rs RunSummary
	return &rs, json.Unmarshal(data, &rs)
}

