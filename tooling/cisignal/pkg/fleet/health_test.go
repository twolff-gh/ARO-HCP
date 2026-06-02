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
	"strings"
	"testing"

	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"
)

func TestComputeHealth(t *testing.T) {
	tests := []struct {
		name     string
		runs     []sippy.CleanedRun
		wantRate float64
		wantRuns int
	}{
		{
			name:     "empty",
			runs:     nil,
			wantRate: 0,
			wantRuns: 0,
		},
		{
			name: "all pass",
			runs: []sippy.CleanedRun{
				{ID: 1, Passed: true},
				{ID: 2, Passed: true},
			},
			wantRate: 100,
			wantRuns: 2,
		},
		{
			name: "mixed",
			runs: []sippy.CleanedRun{
				{ID: 1, Passed: true},
				{ID: 2, Passed: false},
				{ID: 3, Passed: true},
				{ID: 4, Passed: false},
			},
			wantRate: 50,
			wantRuns: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := computeHealth(tt.runs, tt.runs, tt.runs, 14)
			if h.PassRate != tt.wantRate {
				t.Errorf("PassRate = %v, want %v", h.PassRate, tt.wantRate)
			}
			if h.TotalRuns != tt.wantRuns {
				t.Errorf("TotalRuns = %d, want %d", h.TotalRuns, tt.wantRuns)
			}
		})
	}
}

func TestComputeHealth14Day(t *testing.T) {
	windowRuns := []sippy.CleanedRun{
		{ID: 1, Passed: false},
		{ID: 2, Passed: false},
	}
	allRuns := []sippy.CleanedRun{
		{ID: 1, Passed: false},
		{ID: 2, Passed: false},
		{ID: 3, Passed: true},
		{ID: 4, Passed: true},
		{ID: 5, Passed: true},
		{ID: 6, Passed: true},
	}

	h := computeHealth(windowRuns, windowRuns, allRuns, 2)
	if h.PassRate != 0 {
		t.Errorf("PassRate = %v, want 0", h.PassRate)
	}
	if h.TotalRuns != 2 {
		t.Errorf("TotalRuns = %d, want 2", h.TotalRuns)
	}
	wantRate14 := float64(4) / float64(6) * 100
	if h.PassRate14Day != wantRate14 {
		t.Errorf("PassRate14Day = %v, want %v", h.PassRate14Day, wantRate14)
	}
	if h.TotalRuns14Day != 6 {
		t.Errorf("TotalRuns14Day = %d, want 6", h.TotalRuns14Day)
	}
}

func TestComputeHealth14DaySkippedWhenWindowIs14(t *testing.T) {
	runs := []sippy.CleanedRun{
		{ID: 1, Passed: true},
		{ID: 2, Passed: false},
	}
	h := computeHealth(runs, runs, runs, 14)
	if h.PassRate14Day != 0 {
		t.Errorf("PassRate14Day should be 0 when days=14, got %v", h.PassRate14Day)
	}
	if h.TotalRuns14Day != 0 {
		t.Errorf("TotalRuns14Day should be 0 when days=14, got %d", h.TotalRuns14Day)
	}
}

func TestComputeStreak(t *testing.T) {
	tests := []struct {
		name      string
		runs      []sippy.CleanedRun
		wantCount int
		wantState string
		wantSince string
	}{
		{
			name:      "empty",
			runs:      nil,
			wantCount: 0,
		},
		{
			name: "green streak",
			runs: []sippy.CleanedRun{
				{Passed: true, Timestamp: 3000}, {Passed: true, Timestamp: 2000}, {Passed: true, Timestamp: 1000}, {Passed: false, Timestamp: 500},
			},
			wantCount: 3,
			wantState: "green",
			wantSince: "1970-01-01T00:00:01Z",
		},
		{
			name: "red streak with since",
			runs: []sippy.CleanedRun{
				{Passed: false, Timestamp: 1716984000000}, {Passed: false, Timestamp: 1716897600000}, {Passed: true, Timestamp: 1716811200000},
			},
			wantCount: 2,
			wantState: "red",
			wantSince: "2024-05-28T",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := computeStreak(tt.runs)
			if s.Count != tt.wantCount {
				t.Errorf("Count = %d, want %d", s.Count, tt.wantCount)
			}
			if tt.wantState != "" && s.State != tt.wantState {
				t.Errorf("State = %q, want %q", s.State, tt.wantState)
			}
			if tt.wantSince != "" && !strings.HasPrefix(s.Since, tt.wantSince) {
				t.Errorf("Since = %q, want prefix %q", s.Since, tt.wantSince)
			}
		})
	}
}

func TestSelectFailed(t *testing.T) {
	runs := []sippy.CleanedRun{
		{ID: 1, Passed: true},
		{ID: 2, Passed: false, RealFailures: []string{"test-a"}},
		{ID: 3, Passed: false}, // no real failures — infra-only
		{ID: 4, Passed: false, RealFailures: []string{"test-b", "test-c"}},
	}

	got := selectFailed(runs)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 4 {
		t.Errorf("got IDs %d, %d; want 2, 4", got[0].ID, got[1].ID)
	}
}
