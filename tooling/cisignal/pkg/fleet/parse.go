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

// parse.go extracts test errors and pool retry signals from GCS artifacts:
// build-log.txt and extension_test_result JSON.
package fleet

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

const maxErrorLen = 5000 // full error text from test output, before truncation for display

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

type rawTestResult struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Error  string `json:"error"`
	Output string `json:"output"`
}

// testSignals holds per-test data extracted from test result JSON.
type testSignals struct {
	Error       string
	PoolRetries int
	PoolWaitS   float64
}

var (
	poolRetryRe = regexp.MustCompile(`Not enough free identity containers`)
	poolTSRe    = regexp.MustCompile(`"ts"="(\d{4}-\d{2}-\d{2}\s\d{2}:\d{2}:\d{2})`)
)

const tsLayout = "2006-01-02 15:04:05"

// parseTestResults extracts test errors and output-field signals from
// extension_test_result JSON. Returns map of test name to signals
// for failed tests, or nil if parsing fails.
func parseTestResults(data []byte) map[string]*testSignals {
	var tests []rawTestResult
	if err := json.Unmarshal(data, &tests); err != nil {
		slog.Debug("parse: test results decode failed", "error", err)
		return nil
	}
	return extractSignals(tests)
}

// parseBuildLog extracts test results from JSON embedded in
// build-log.txt. Fallback when extension_test_result is unavailable.
func parseBuildLog(data []byte) map[string]*testSignals {
	idx := bytes.Index(data, []byte("\n["))
	if idx >= 0 {
		idx++
	} else if len(data) > 0 && data[0] == '[' {
		idx = 0
	} else {
		return nil
	}

	var tests []rawTestResult
	if err := json.NewDecoder(bytes.NewReader(data[idx:])).Decode(&tests); err != nil {
		slog.Debug("parse: build-log decode failed", "error", err)
		return nil
	}
	return extractSignals(tests)
}

func extractSignals(tests []rawTestResult) map[string]*testSignals {
	result := map[string]*testSignals{}
	for _, t := range tests {
		if t.Result != "failed" || t.Error == "" {
			continue
		}
		sig := &testSignals{
			Error: truncate(t.Error, maxErrorLen),
		}
		if t.Output != "" {
			extractOutputSignals(t.Output, sig)
		}
		result[t.Name] = sig
	}
	return result
}

func extractOutputSignals(output string, sig *testSignals) {
	var firstRetry, lastRetry time.Time

	for _, line := range strings.Split(output, "\n") {
		if poolRetryRe.MatchString(line) {
			sig.PoolRetries++
			if m := poolTSRe.FindStringSubmatch(line); len(m) > 1 {
				if ts, err := time.Parse(tsLayout, m[1]); err == nil {
					if firstRetry.IsZero() {
						firstRetry = ts
					}
					lastRetry = ts
				}
			}
		}
	}

	if sig.PoolRetries > 0 && !firstRetry.IsZero() && !lastRetry.IsZero() {
		sig.PoolWaitS = lastRetry.Sub(firstRetry).Seconds()
	}
}
