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

package fleet

import (
	"testing"
)

func TestParseBuildLog(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr string
	}{
		{
			name: "with shell preamble",
			input: `+ export FOO=bar
+ az login --service-principal
+ ./test/run-suite prod/parallel
[
  {"name": "test-a", "result": "passed", "output": "ok"},
  {"name": "test-b", "result": "failed", "error": "timeout exceeded", "output": "fail log"},
  {"name": "test-c", "result": "skipped"}
]
`,
			wantLen: 1,
			wantErr: "timeout exceeded",
		},
		{
			name: "json array only",
			input: `[
  {"name": "test-a", "result": "failed", "error": "x509: cert error"},
  {"name": "test-b", "result": "failed", "error": "connection refused"}
]`,
			wantLen: 2,
			wantErr: "x509: cert error",
		},
		{
			name: "trailing text after array",
			input: `+ setup
[
  {"name": "test-a", "result": "failed", "error": "boom"}
]
Error: 1 test failed
1 test failed
`,
			wantLen: 1,
			wantErr: "boom",
		},
		{
			name:    "empty input",
			input:   "",
			wantLen: 0,
		},
		{
			name:    "no json array",
			input:   "just shell output\nno tests here\n",
			wantLen: 0,
		},
		{
			name: "no failures",
			input: `[
  {"name": "test-a", "result": "passed"},
  {"name": "test-b", "result": "skipped"}
]`,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBuildLog([]byte(tt.input))
			if tt.wantLen == 0 {
				if len(got) != 0 {
					t.Errorf("want empty map, got %d entries", len(got))
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(got), tt.wantLen)
			}
			if tt.wantErr != "" {
				found := false
				for _, v := range got {
					if v.Error == tt.wantErr {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error %q not found", tt.wantErr)
				}
			}
		})
	}
}

func TestParseTestResults(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr string
	}{
		{
			name: "clean JSON array",
			input: `[
  {"name": "test-a", "result": "passed", "duration": 5000},
  {"name": "test-b", "result": "failed", "error": "timeout exceeded", "output": "fail log", "duration": 120000},
  {"name": "test-c", "result": "skipped"}
]`,
			wantLen: 1,
			wantErr: "timeout exceeded",
		},
		{
			name:    "empty array",
			input:   `[]`,
			wantLen: 0,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantLen: 0,
		},
		{
			name:    "with output signals",
			input:   `[{"name": "test-x", "result": "failed", "error": "context deadline exceeded", "output": "\"ts\"=\"2026-05-28 10:20:18\" \"level\"=0 \"msg\"=\"Not enough free identity containers\"\n", "duration": 1200000}]`,
			wantLen: 1,
			wantErr: "context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTestResults([]byte(tt.input))
			if tt.wantLen == 0 {
				if len(got) != 0 {
					t.Errorf("want empty map, got %d entries", len(got))
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(got), tt.wantLen)
			}
			for _, v := range got {
				if tt.wantErr != "" && v.Error != tt.wantErr {
					t.Errorf("Error = %q, want %q", v.Error, tt.wantErr)
				}
			}
		})
	}
}

func TestExtractOutputSignals_PoolRetries(t *testing.T) {
	output := `"ts"="2026-05-28 10:20:18" "level"=0 "msg"="Not enough free identity containers"
"ts"="2026-05-28 10:25:18" "level"=0 "msg"="Not enough free identity containers"
"ts"="2026-05-28 10:30:18" "level"=0 "msg"="Not enough free identity containers"
`
	sig := &testSignals{}
	extractOutputSignals(output, sig)
	if sig.PoolRetries != 3 {
		t.Errorf("PoolRetries = %d, want 3", sig.PoolRetries)
	}
	if sig.PoolWaitS != 600 {
		t.Errorf("PoolWaitS = %f, want 600", sig.PoolWaitS)
	}
}
