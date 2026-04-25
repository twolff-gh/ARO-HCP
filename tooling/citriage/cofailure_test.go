package main

import (
	"testing"
)

func TestUnionFind(t *testing.T) {
	uf := newUnionFind()

	if uf.find("a") != "a" {
		t.Errorf("find(a) = %s, want a", uf.find("a"))
	}

	uf.union("a", "b")
	if uf.find("a") != uf.find("b") {
		t.Errorf("after union(a,b): find(a)=%s find(b)=%s, want same", uf.find("a"), uf.find("b"))
	}

	uf.union("b", "c")
	if uf.find("a") != uf.find("c") {
		t.Errorf("after union(b,c): find(a)=%s find(c)=%s, want same", uf.find("a"), uf.find("c"))
	}

	if uf.find("d") == uf.find("a") {
		t.Error("d should not be in same group as a")
	}
}

func TestSetIntersectionCount(t *testing.T) {
	tests := []struct {
		desc string
		a, b map[int64]bool
		want int
	}{
		{"empty sets", map[int64]bool{}, map[int64]bool{}, 0},
		{"no overlap", map[int64]bool{1: true, 2: true}, map[int64]bool{3: true, 4: true}, 0},
		{"full overlap", map[int64]bool{1: true, 2: true}, map[int64]bool{1: true, 2: true}, 2},
		{"partial overlap", map[int64]bool{1: true, 2: true, 3: true}, map[int64]bool{2: true, 3: true, 4: true}, 2},
		{"one empty", map[int64]bool{1: true}, map[int64]bool{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := setIntersectionCount(tt.a, tt.b); got != tt.want {
				t.Errorf("setIntersectionCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildCoFailureGroups_Basic(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB", "TestC"},
			Annotations: map[string]string{ev2HashAnnotation: "hash1", ev2RegionAnnotation: "eastus"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB", "TestC"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestD"}},
		{ID: 5, Timestamp: 5000, FailedTestNames: []string{"TestD"}},
	}
	groups, _ := buildCoFailureGroups(runs, 0.8, 0)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	g := groups[0]
	if len(g.Members) != 3 {
		t.Errorf("expected 3 members, got %d", len(g.Members))
	}
	if g.Leader != "TestA" && g.Leader != "TestB" {
		t.Errorf("leader = %s, expected TestA or TestB (both have 3 failures)", g.Leader)
	}
	for _, m := range g.Members {
		if m.Test == "TestD" {
			t.Error("TestD should not be in the co-failure group")
		}
	}

	if len(g.CommonEV2) != 1 || g.CommonEV2[0] != "hash1" {
		t.Errorf("CommonEV2 = %v, want [hash1]", g.CommonEV2)
	}
	if len(g.CommonRegions) != 1 || g.CommonRegions[0] != "eastus" {
		t.Errorf("CommonRegions = %v, want [eastus]", g.CommonRegions)
	}
	if g.Onset == "" {
		t.Error("Onset should not be empty")
	}
}

func TestBuildCoFailureGroups_NoGroups(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestB"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestB"}},
	}

	groups, stats := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(groups))
	}
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.EligibleTests != 2 {
		t.Errorf("eligible_tests = %d, want 2", stats.EligibleTests)
	}
	if stats.MaxOverlapPct != 0 {
		t.Errorf("max_overlap_pct = %d, want 0 (no shared runs)", stats.MaxOverlapPct)
	}
	if stats.Reason == "" {
		t.Error("expected non-empty reason when no groups formed")
	}
}

func TestBuildCoFailureGroups_Disabled(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB"}},
	}

	groups, stats := buildCoFailureGroups(runs, 0, 0)
	if groups != nil {
		t.Errorf("expected nil with threshold=0, got %v", groups)
	}
	if stats != nil {
		t.Errorf("expected nil stats with threshold=0, got %v", stats)
	}
}

func TestBuildCoFailureGroups_BelowThreshold(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestA"}},
		{ID: 5, Timestamp: 5000, FailedTestNames: []string{"TestA"}},
		{ID: 6, Timestamp: 6000, FailedTestNames: []string{"TestB"}},
		{ID: 7, Timestamp: 7000, FailedTestNames: []string{"TestB"}},
		{ID: 8, Timestamp: 8000, FailedTestNames: []string{"TestB"}},
	}

	groups, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (overlap 2/5=0.4 < 0.8), got %d", len(groups))
	}
}

func TestBuildCoFailureGroups_SoloFailures(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 5, Timestamp: 5000, FailedTestNames: []string{"TestA"}},
	}

	groups, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	for _, m := range groups[0].Members {
		switch m.Test {
		case "TestA":
			if m.TotalFails != 5 || m.CoFails != 4 || m.SoloFails != 1 {
				t.Errorf("TestA: total=%d co=%d solo=%d, want 5/4/1",
					m.TotalFails, m.CoFails, m.SoloFails)
			}
		case "TestB":
			if m.TotalFails != 4 || m.CoFails != 4 || m.SoloFails != 0 {
				t.Errorf("TestB: total=%d co=%d solo=%d, want 4/4/0",
					m.TotalFails, m.CoFails, m.SoloFails)
			}
		}
	}
}

func TestBuildCoFailureGroups_SyntheticFiltered(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"[sig-sippy] infra", "TestA"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"[sig-sippy] infra", "TestA"}},
	}

	groups, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (synthetic should be filtered), got %d", len(groups))
	}
}

func TestBuildCoFailureGroups_TwoSeparateGroups(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestC", "TestD"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestC", "TestD"}},
	}

	groups, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	groupTests := map[string]bool{}
	for _, g := range groups {
		for _, m := range g.Members {
			groupTests[m.Test] = true
		}
	}
	for _, name := range []string{"TestA", "TestB", "TestC", "TestD"} {
		if !groupTests[name] {
			t.Errorf("%s should be in a co-failure group", name)
		}
	}
}

func TestBuildCoFailureGroups_SingleFailureTestExcluded(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA"}},
	}

	groups, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (TestB only has 1 failure), got %d", len(groups))
	}
}

func TestBuildCoFailureGroups_BlastRadius(t *testing.T) {
	// Two catastrophic runs (>3 failures) link TestA-TestE together.
	// Two normal runs provide genuine TestA+TestB co-failure signal.
	// With blast=3, the catastrophic runs are excluded, so only TestA+TestB form a group.
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB", "TestC", "TestD", "TestE"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB", "TestC", "TestD", "TestE"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestA", "TestB"}},
	}

	// Without blast radius: all 5 tests form one group
	groupsNoBR, _ := buildCoFailureGroups(runs, 0.8, 0)
	if len(groupsNoBR) != 1 {
		t.Fatalf("without blast radius: expected 1 group, got %d", len(groupsNoBR))
	}
	if len(groupsNoBR[0].Members) != 5 {
		t.Errorf("without blast radius: expected 5 members, got %d", len(groupsNoBR[0].Members))
	}

	// With blast=3: catastrophic runs excluded, only TestA+TestB remain as co-failure
	groupsBR, stats := buildCoFailureGroups(runs, 0.8, 3)
	if stats.BlastExcluded != 2 {
		t.Errorf("expected 2 blast-excluded runs, got %d", stats.BlastExcluded)
	}
	if len(groupsBR) != 1 {
		t.Fatalf("with blast radius: expected 1 group (TestA+TestB), got %d", len(groupsBR))
	}
	if len(groupsBR[0].Members) != 2 {
		t.Errorf("with blast radius: expected 2 members, got %d", len(groupsBR[0].Members))
	}
	for _, m := range groupsBR[0].Members {
		if m.Test != "TestA" && m.Test != "TestB" {
			t.Errorf("unexpected member %s in blast-filtered group", m.Test)
		}
	}
}

func TestBuildCoFailureGroups_BlastRadiusDisabled(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB", "TestC"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB", "TestC"}},
	}

	// blast=0 means disabled — should behave same as without blast
	groups, stats := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 1 || len(groups[0].Members) != 3 {
		t.Errorf("blast=0 should not filter; got %d groups", len(groups))
	}
	if stats.BlastExcluded != 0 {
		t.Errorf("blast=0: expected 0 excluded, got %d", stats.BlastExcluded)
	}
}

func TestBuildCoFailureGroups_AdaptiveThreshold(t *testing.T) {
	// TestA fails in runs {1,2,3}, TestB fails in runs {1,3,4}
	// Intersection = {1,3} = 2, min(3,3) = 3 → overlap = 2/3 ≈ 66%
	// At threshold=0.8, 66% < 80% → no group normally.
	// With adaptive threshold (max 3 runs/test ≤ 5), threshold drops to 0.5 → group forms.
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestB"}},
	}

	groups, stats := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 1 {
		t.Fatalf("adaptive threshold should form 1 group (66%% >= 50%%), got %d", len(groups))
	}
	if stats.AdaptedThreshold != 0.5 {
		t.Errorf("AdaptedThreshold = %v, want 0.5", stats.AdaptedThreshold)
	}
	if stats.MaxOverlapPct != 66 {
		t.Errorf("MaxOverlapPct = %d, want 66", stats.MaxOverlapPct)
	}
}

func TestBuildCoFailureGroups_NoAdaptationWithManyRuns(t *testing.T) {
	// When tests have >5 runs, no adaptation occurs
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestA"}},
		{ID: 4, Timestamp: 4000, FailedTestNames: []string{"TestA"}},
		{ID: 5, Timestamp: 5000, FailedTestNames: []string{"TestA"}},
		{ID: 6, Timestamp: 6000, FailedTestNames: []string{"TestA"}},
		{ID: 7, Timestamp: 7000, FailedTestNames: []string{"TestB"}},
	}

	// TestA has 6 runs (>5), so no adaptation. Overlap = 2/3 = 66% < 80%
	groups, stats := buildCoFailureGroups(runs, 0.8, 0)
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (no adaptation with >5 runs), got %d", len(groups))
	}
	if stats.AdaptedThreshold != 0 {
		t.Errorf("AdaptedThreshold should be 0 (no adaptation), got %v", stats.AdaptedThreshold)
	}
}

func TestBuildCoFailureGroups_NoAdaptationWhenThresholdAlreadyLow(t *testing.T) {
	// When user already sets threshold ≤ 0.5, no adaptation needed
	runs := []JobRun{
		{ID: 1, Timestamp: 1000, FailedTestNames: []string{"TestA", "TestB"}},
		{ID: 2, Timestamp: 2000, FailedTestNames: []string{"TestA"}},
		{ID: 3, Timestamp: 3000, FailedTestNames: []string{"TestB"}},
	}

	groups, stats := buildCoFailureGroups(runs, 0.5, 0)
	if stats.AdaptedThreshold != 0 {
		t.Errorf("should not adapt when threshold already ≤ 0.5, got adapted=%v", stats.AdaptedThreshold)
	}
	if len(groups) != 1 {
		t.Errorf("expected 1 group at 0.5 threshold (50%% overlap), got %d", len(groups))
	}
}
