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
	"bytes"
	"strings"
	"testing"
)

func TestWriteTriageInputFlatTable(t *testing.T) {
	r := &Result{
		Env:  "dev",
		Days: 3,
		Health: Health{
			PassRate:  46.5,
			TotalRuns: 155,
			Streak:    Streak{Count: 2, State: "red"},
		},
		RunSummary: &RunSummary{
			TotalRuns: 80, PRRuns: 78, PeriodicRuns: 2,
		},
		FailingTests: []FailingTest{
			{
				Test:         "BackendControllerRetryHotLoop alert",
				Hits:         3,
				PreHits:      0,
				PeriodicHits: 3,
				PassRate:     56.2,
				ErrorSamples: []ErrorSample{
					{Text: "retry ratio > 50% operationclustercreate", Count: 2, URLs: []string{"https://prow.ci/run/1", "https://prow.ci/run/2"}},
					{Text: "retry ratio > 50% deleteorphanedmaestroreadonlybundles", Count: 1, URLs: []string{"https://prow.ci/run/3"}},
				},
			},
			{
				Test:         "Customer should create HCP cluster A",
				Hits:         5,
				PreHits:      3,
				PeriodicHits: 2,
				PRNumbers:    []int{5436, 5442},
				PassRate:     90.0,
				ErrorSamples: []ErrorSample{
					{Text: "failed to create HCP cluster arm64-vm-hcp-cluster, caused by: timeout '20.000000' minutes exceeded during CreateHCPClusterFromParam for cluster arm64-vm-hcp-cluster in resource group arm64-vm-cluster-n88xqf, error: context deadline exceeded", Count: 3, URLs: []string{"https://prow.ci/run/4", "https://prow.ci/run/5", "https://prow.ci/run/6"}, PRNumbers: []int{5436}},
					{Text: "ResourceGroupQuotaExceeded for rg-a", Count: 2, URLs: []string{"https://prow.ci/run/7", "https://prow.ci/run/8"}, PRNumbers: []int{5442}},
				},
				PoolRetries: 30,
				PoolWaitS:   1800,
			},
		},
	}

	var buf bytes.Buffer
	WriteTriageInput(&buf, r)
	out := buf.String()

	// Table header — Pre/Per/PRs columns
	if !strings.Contains(out, "| # | Test | Pre | Per | PRs | Pass% | Pool | Wait |") {
		t.Error("table header missing or wrong")
	}

	// All tests in table
	if !strings.Contains(out, "BackendControllerRetryHotLoop") {
		t.Error("alert test missing from table")
	}
	if !strings.Contains(out, "Customer should create HCP cluster A") {
		t.Error("test A missing from table")
	}

	// Pre/Per columns — periodic-only test shows 0 pre
	if !strings.Contains(out, "| 0 | 3 |  |") {
		t.Error("periodic-only test should show Pre=0, Per=3, no PRs")
	}
	// Pre/Per columns — mixed test shows both + PR numbers
	if !strings.Contains(out, "| 3 | 2 | #5436,#5442 |") {
		t.Error("mixed test should show Pre=3, Per=2, PRs=#5436,#5442")
	}

	// Error section headers show pre + per breakdown
	if !strings.Contains(out, "(0 pre + 3 per, 56.2%)") {
		t.Error("error header should show pre + per breakdown for periodic-only test")
	}
	if !strings.Contains(out, "(3 pre + 2 per, 90.0%)") {
		t.Error("error header should show pre + per breakdown for mixed test")
	}

	// Errors section — distinct shapes present
	if !strings.Contains(out, "## Errors") {
		t.Error("errors section missing")
	}
	if !strings.Contains(out, "operationclustercreate") {
		t.Error("operationclustercreate error missing")
	}
	if !strings.Contains(out, "deleteorphanedmaestroreadonlybundles") {
		t.Error("deleteorphaned error missing")
	}
	if !strings.Contains(out, "CreateHCPClusterFromParam") {
		t.Error("timeout error missing for test A")
	}
	if !strings.Contains(out, "ResourceGroupQuotaExceeded") {
		t.Error("quota error missing for test A")
	}

	// Dedup counts shown for repeated shapes
	if !strings.Contains(out, "(x2)") {
		t.Error("expected (x2) for operationclustercreate or timeout B")
	}
	if !strings.Contains(out, "(x3)") {
		t.Error("expected (x3) for timeout error")
	}

	// First run URL shown per shape (not all URLs)
	if !strings.Contains(out, "Run: https://prow.ci/run/1") {
		t.Error("first run URL missing for operationclustercreate shape")
	}
	if !strings.Contains(out, "Run: https://prow.ci/run/4") {
		t.Error("first run URL missing for timeout shape")
	}
}

func TestDedupKey(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{
			name: "identical timeout different RG suffix",
			a:    `failed to create HCP cluster arm64-vm-hcp-cluster, caused by: timeout '20.000000' minutes exceeded during CreateHCPClusterFromParam for cluster arm64-vm-hcp-cluster in resource group arm64-vm-cluster-n88xqf, error: context deadline exceeded`,
			b:    `failed to create HCP cluster arm64-vm-hcp-cluster, caused by: timeout '20.000000' minutes exceeded during CreateHCPClusterFromParam for cluster arm64-vm-hcp-cluster in resource group arm64-vm-cluster-g9p59c, error: context deadline exceeded`,
			same: true,
		},
		{
			name: "quota different count and RG suffix",
			a:    `failed to create resource group "rg-zstream-upgrade-4-20-fjlzkb": ResourceGroupQuotaExceeded: count is '980'`,
			b:    `failed to create resource group "rg-zstream-upgrade-4-20-lw65v2": ResourceGroupQuotaExceeded: count is '984'`,
			same: true,
		},
		{
			name: "timeout vs quota are different",
			a:    `failed to create HCP cluster arm64-vm-hcp-cluster, caused by: timeout '20.000000' minutes exceeded`,
			b:    `ResourceGroupQuotaExceeded: count is '980'`,
			same: false,
		},
		{
			name: "different workqueue names stay different",
			a:    `retry ratio > 50% operationclustercreate`,
			b:    `retry ratio > 50% deleteorphanedmaestroreadonlybundles`,
			same: false,
		},
		{
			name: "UUID stripped",
			a:    `HostedCluster ID: cf1ac5e9-d459-400d-af77-93d87b7e6390 KubeAPIServer availability is below SLO`,
			b:    `HostedCluster ID: df781bad-2066-47e0-83ab-1ba3acf01eef KubeAPIServer availability is below SLO`,
			same: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ka := dedupKey(tt.a)
			kb := dedupKey(tt.b)
			if (ka == kb) != tt.same {
				t.Errorf("dedupKey equality=%v, want %v\n  a: %s\n  b: %s", ka == kb, tt.same, ka, kb)
			}
		})
	}
}

func TestWriteTriageInputNoFailures(t *testing.T) {
	r := &Result{
		Env:  "prod",
		Days: 2,
		Health: Health{
			PassRate:  100,
			TotalRuns: 10,
			Streak:    Streak{Count: 10, State: "green"},
		},
	}

	var buf bytes.Buffer
	WriteTriageInput(&buf, r)
	out := buf.String()

	if !strings.Contains(out, "No failing tests.") {
		t.Error("missing 'no failing tests' message")
	}
}

func TestFormatStreak(t *testing.T) {
	tests := []struct {
		name string
		s    Streak
		want string
	}{
		{"zero", Streak{}, "Streak: 0"},
		{"green", Streak{Count: 3, State: "green", Since: "2026-05-25T00:00:00Z"}, "Streak: 3 green"},
		{"red with since", Streak{Count: 5, State: "red", Since: "2026-05-25T12:00:00Z"}, "Streak: 5 red since 2026-05-25"},
		{"red no since", Streak{Count: 1, State: "red"}, "Streak: 1 red"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatStreak(tt.s)
			if got != tt.want {
				t.Errorf("formatStreak() = %q, want %q", got, tt.want)
			}
		})
	}
}
