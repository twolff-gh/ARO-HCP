// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package sippy queries the Sippy API for CI job run data, partitions runs
// by EV2 deployment hash vs nightly/cron, cleans synthetic test names, and
// provides per-run risk analysis and summary endpoints.
package sippy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	baseURL          = "https://sippy.dptools.openshift.org"
	httpTimeout      = 30 * time.Second
	maxResponseSize  = 50 << 20 // 50 MB — largest observed response is ~78KB (/api/jobs/analysis), cap is defensive
	maxErrorBodySize = 4096     // enough to capture Sippy error messages without reading unbounded responses
)

type envConfig struct {
	release   string // Sippy release name (e.g., "aro-production")
	jobFilter string // job name substring for Sippy filter (e.g., "prod-e2e-parallel")
}

var envConfigs = map[string]envConfig{
	"int":  {release: "aro-integration", jobFilter: "integration-e2e-parallel"},
	"stg":  {release: "aro-stage", jobFilter: "stage-e2e-parallel"},
	"prod": {release: "aro-production", jobFilter: "prod-e2e-parallel"},
	"dev":  {release: "Presubmits", jobFilter: "main-e2e-parallel"},
}

type Client struct {
	c *http.Client
}

func NewClient() *Client {
	return &Client{c: &http.Client{Timeout: httpTimeout}}
}

func (s *Client) get(path string, params url.Values) ([]byte, error) {
	reqURL := baseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}
	resp, err := s.c.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("sippy %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
		return nil, fmt.Errorf("sippy %s: %d %s", path, resp.StatusCode, body)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
}

// maxRunsPerQuery caps the Sippy pagination response. Sippy returns runs newest-first;
// at ~3-5 runs/day per env, 1000 covers ~200-300 days.
const maxRunsPerQuery = 1000

type sippyFilter struct {
	Items        []sippyFilterItem `json:"items"`
	LinkOperator string            `json:"linkOperator"`
}

type sippyFilterItem struct {
	ColumnField   string `json:"columnField"`
	OperatorValue string `json:"operatorValue"`
	Value         string `json:"value"`
}

func (s *Client) ListRuns(release string, jobFilter string, since time.Time) ([]rawRun, error) {
	filter := sippyFilter{
		Items: []sippyFilterItem{
			{ColumnField: "job", OperatorValue: "contains", Value: jobFilter},
			{ColumnField: "timestamp", OperatorValue: ">", Value: fmt.Sprintf("%d", since.UnixMilli())},
		},
		LinkOperator: "and",
	}
	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return nil, fmt.Errorf("marshal filter: %w", err)
	}

	data, err := s.get("/api/jobs/runs", url.Values{
		"release": {release},
		"limit":   {fmt.Sprintf("%d", maxRunsPerQuery)},
		"filter":  {string(filterJSON)},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Rows []rawRun `json:"rows"`
	}
	return resp.Rows, json.Unmarshal(data, &resp)
}

// RunSummary holds the fields we extract from /api/job/run/summary.
type RunSummary struct {
	TestFailures    map[string]string `json:"testFailures"`
	DurationSeconds int               `json:"durationSeconds"`
	TestCount       int               `json:"testCount"`
}

// FetchRunSummary fetches the run summary including failures and duration.
func (s *Client) FetchRunSummary(runID string) (RunSummary, error) {
	data, err := s.get("/api/job/run/summary", url.Values{"prow_job_run_id": {runID}})
	if err != nil {
		return RunSummary{}, err
	}
	var resp RunSummary
	if err := json.Unmarshal(data, &resp); err != nil {
		return RunSummary{}, err
	}
	if resp.TestFailures == nil {
		resp.TestFailures = map[string]string{}
	}
	return resp, nil
}

// ValidEnv returns true if the environment name is recognized.
func ValidEnv(env string) bool {
	_, ok := envConfigs[env]
	return ok
}

// EnvConfig returns the Sippy release and job filter for an environment.
func EnvConfig(env string) (release, jobFilter string, err error) {
	cfg, ok := envConfigs[env]
	if !ok {
		return "", "", fmt.Errorf("unknown env %q (valid: int, stg, prod, dev)", env)
	}
	return cfg.release, cfg.jobFilter, nil
}
