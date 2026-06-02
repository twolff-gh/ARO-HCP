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

package ops

import "testing"

func TestExtractTail(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		lines  int
		maxLen int
		want   string
	}{
		{
			name:   "basic",
			input:  "line1\nline2\nline3\nline4\nline5",
			lines:  3,
			maxLen: 500,
			want:   "line3\nline4\nline5",
		},
		{
			name:   "skip empty lines",
			input:  "line1\n\n\nline2\n\nline3\n",
			lines:  2,
			maxLen: 500,
			want:   "line2\nline3",
		},
		{
			name:   "truncate to maxLen from end",
			input:  "short\nthis is a longer line that should be kept",
			lines:  5,
			maxLen: 20,
			want:   " that should be kept",
		},
		{
			name:   "empty input",
			input:  "",
			lines:  3,
			maxLen: 500,
			want:   "",
		},
		{
			name:   "fewer lines than requested",
			input:  "only\ntwo",
			lines:  5,
			maxLen: 500,
			want:   "only\ntwo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTail(tt.input, tt.lines, tt.maxLen)
			if got != tt.want {
				t.Errorf("extractTail() = %q, want %q", got, tt.want)
			}
		})
	}
}
