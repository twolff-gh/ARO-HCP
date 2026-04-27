package main

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	maxCommonRunIDs       = 10
	minCoFailRunCount     = 2   // test must fail in >=N runs to be eligible
	minCoFailGroupSize    = 2   // minimum members to form a group
	sparseDataMaxRuns     = 5   // adapt threshold when max runs/test is at or below this
	adaptedThresholdFloor = 0.5 // lowered threshold for sparse data
)

type coFailureGroupJSON struct {
	Leader              string                `json:"leader"`
	Members             []coFailureMemberJSON `json:"members"`
	MinOverlapPct       int                   `json:"min_overlap_pct"`
	DistinctRuns        int                   `json:"distinct_runs"`
	CommonRunIDs        []int64               `json:"common_run_ids"`
	CommonRunsTruncated bool                  `json:"common_runs_truncated,omitempty"`
	CommonEV2           []string              `json:"common_ev2_hashes,omitempty"`
	CommonRegions       []string              `json:"common_regions,omitempty"`
	Onset               string                `json:"onset,omitempty"`
}

type coFailureStatsJSON struct {
	Threshold         float64 `json:"threshold"`
	AdaptedThreshold  float64 `json:"adapted_threshold,omitempty"`
	TotalFailingTests int     `json:"total_failing_tests"`
	EligibleTests     int     `json:"eligible_tests"`
	PairsChecked      int     `json:"pairs_checked"`
	PairsAbove        int     `json:"pairs_above_threshold"`
	MaxOverlapPct     int     `json:"max_overlap_pct"`
	Reason            string  `json:"reason,omitempty"`
}

type coFailureMemberJSON struct {
	Test       string `json:"test"`
	TotalFails int    `json:"total_failures"`
	CoFails    int    `json:"co_failures"`
	SoloFails  int    `json:"solo_failures"`
}

type unionFind struct {
	parent map[string]string
	rank   map[string]int
}

func newUnionFind() *unionFind {
	return &unionFind{parent: map[string]string{}, rank: map[string]int{}}
}

func (uf *unionFind) find(x string) string {
	if _, ok := uf.parent[x]; !ok {
		uf.parent[x] = x
	}
	if uf.parent[x] != x {
		uf.parent[x] = uf.find(uf.parent[x])
	}
	return uf.parent[x]
}

func (uf *unionFind) union(x, y string) {
	rx, ry := uf.find(x), uf.find(y)
	if rx == ry {
		return
	}
	if uf.rank[rx] < uf.rank[ry] {
		rx, ry = ry, rx
	}
	uf.parent[ry] = rx
	if uf.rank[rx] == uf.rank[ry] {
		uf.rank[rx]++
	}
}

type coFailureEdge struct {
	a, b    string
	overlap float64
}

// buildTestRunMap indexes which runs each test failed in.
func buildTestRunMap(runs []JobRun) map[string]map[int64]bool {
	testRuns := map[string]map[int64]bool{}
	for _, r := range runs {
		for _, name := range r.FailedTestNames {
			if isSyntheticTest(name) {
				continue
			}
			if testRuns[name] == nil {
				testRuns[name] = map[int64]bool{}
			}
			testRuns[name][r.ID] = true
		}
	}
	return testRuns
}

// buildCoFailureGroups computes pairwise overlap between failing tests across runs
// and clusters them via union-find when overlap exceeds the threshold.
func buildCoFailureGroups(runs []JobRun, threshold float64) ([]coFailureGroupJSON, *coFailureStatsJSON) {
	if threshold <= 0 {
		return nil, nil
	}

	testRuns := buildTestRunMap(runs)

	var tests []string
	for name, rids := range testRuns {
		if len(rids) >= minCoFailRunCount {
			tests = append(tests, name)
		}
	}
	slices.Sort(tests)

	totalFailing := len(testRuns)

	if len(tests) < minCoFailGroupSize {
		return nil, &coFailureStatsJSON{
			Threshold:         threshold,
			TotalFailingTests: totalFailing,
			EligibleTests:     len(tests),
			Reason:            fmt.Sprintf("%d failing tests, %d with >=%d runs", totalFailing, len(tests), minCoFailRunCount),
		}
	}

	effectiveThreshold := threshold
	maxRunsPerTest := 0
	for _, name := range tests {
		if n := len(testRuns[name]); n > maxRunsPerTest {
			maxRunsPerTest = n
		}
	}
	adapted := maxRunsPerTest > 0 && maxRunsPerTest <= sparseDataMaxRuns && threshold > adaptedThresholdFloor
	if adapted {
		effectiveThreshold = adaptedThresholdFloor
	}

	uf := newUnionFind()
	var edges []coFailureEdge
	pairsChecked := 0
	pairsAbove := 0
	maxOverlap := 0.0
	for i := 0; i < len(tests); i++ {
		for j := i + 1; j < len(tests); j++ {
			inter := setIntersectionCount(testRuns[tests[i]], testRuns[tests[j]])
			if inter == 0 {
				continue
			}
			pairsChecked++
			denom := min(len(testRuns[tests[i]]), len(testRuns[tests[j]]))
			overlap := float64(inter) / float64(denom)
			if overlap > maxOverlap {
				maxOverlap = overlap
			}
			if overlap >= effectiveThreshold {
				pairsAbove++
				uf.union(tests[i], tests[j])
				edges = append(edges, coFailureEdge{tests[i], tests[j], overlap})
			}
		}
	}

	stats := &coFailureStatsJSON{
		Threshold:         threshold,
		TotalFailingTests: totalFailing,
		EligibleTests:     len(tests),
		PairsChecked:      pairsChecked,
		PairsAbove:        pairsAbove,
		MaxOverlapPct:     int(maxOverlap * 100),
	}
	if adapted {
		stats.AdaptedThreshold = effectiveThreshold
	}
	if pairsAbove == 0 {
		stats.Reason = fmt.Sprintf("best overlap %d%% is below %d%% threshold", int(maxOverlap*100), int(effectiveThreshold*100))
		if adapted {
			stats.Reason += fmt.Sprintf(" (adapted from %d%% due to sparse data: max %d runs/test)", int(threshold*100), maxRunsPerTest)
		}
	}

	groupMembers := map[string][]string{}
	for _, name := range tests {
		root := uf.find(name)
		groupMembers[root] = append(groupMembers[root], name)
	}

	runByID := map[int64]JobRun{}
	for _, r := range runs {
		runByID[r.ID] = r
	}

	var result []coFailureGroupJSON
	for _, members := range groupMembers {
		if len(members) < minCoFailGroupSize {
			continue
		}
		result = append(result, assembleCoFailureGroup(members, testRuns, edges, uf, runByID))
	}

	slices.SortFunc(result, func(a, b coFailureGroupJSON) int {
		totalA, totalB := 0, 0
		for _, m := range a.Members {
			totalA += m.TotalFails
		}
		for _, m := range b.Members {
			totalB += m.TotalFails
		}
		if totalA != totalB {
			return cmp.Compare(totalB, totalA)
		}
		return cmp.Compare(a.Leader, b.Leader)
	})

	return result, stats
}

func assembleCoFailureGroup(
	members []string,
	testRuns map[string]map[int64]bool,
	edges []coFailureEdge,
	uf *unionFind,
	runByID map[int64]JobRun,
) coFailureGroupJSON {
	leader := members[0]
	for _, m := range members[1:] {
		if len(testRuns[m]) > len(testRuns[leader]) {
			leader = m
		}
	}

	groupRoot := uf.find(members[0])
	minOverlap := 1.0
	for _, e := range edges {
		if uf.find(e.a) == groupRoot {
			minOverlap = min(minOverlap, e.overlap)
		}
	}

	runMemberCount := map[int64]int{}
	for _, m := range members {
		for rid := range testRuns[m] {
			runMemberCount[rid]++
		}
	}
	var commonRuns []int64
	for rid, count := range runMemberCount {
		if count >= minCoFailRunCount {
			commonRuns = append(commonRuns, rid)
		}
	}
	slices.SortFunc(commonRuns, func(a, b int64) int {
		return cmp.Compare(runByID[b].Timestamp, runByID[a].Timestamp)
	})

	ev2Set := map[string]bool{}
	regionSet := map[string]bool{}
	var onset int64
	for _, rid := range commonRuns {
		r := runByID[rid]
		if h := ev2Hash(r); h != "" {
			ev2Set[h] = true
		}
		if reg := ev2Region(r); reg != "" {
			regionSet[reg] = true
		}
		if onset == 0 || r.Timestamp < onset {
			onset = r.Timestamp
		}
	}

	outputRuns := commonRuns
	commonRunsTruncated := len(outputRuns) > maxCommonRunIDs
	if commonRunsTruncated {
		outputRuns = outputRuns[:maxCommonRunIDs]
	}

	slices.SortFunc(members, func(a, b string) int {
		if a == leader {
			return -1
		}
		if b == leader {
			return 1
		}
		return cmp.Compare(len(testRuns[b]), len(testRuns[a]))
	})

	memberDetails := make([]coFailureMemberJSON, len(members))
	for i, m := range members {
		total := len(testRuns[m])
		co := 0
		for rid := range testRuns[m] {
			for _, other := range members {
				if other != m && testRuns[other][rid] {
					co++
					break
				}
			}
		}
		memberDetails[i] = coFailureMemberJSON{
			Test:       m,
			TotalFails: total,
			CoFails:    co,
			SoloFails:  total - co,
		}
	}

	onsetStr := ""
	if onset > 0 {
		onsetStr = time.UnixMilli(onset).UTC().Format(time.RFC3339)
	}

	return coFailureGroupJSON{
		Leader:              leader,
		Members:             memberDetails,
		MinOverlapPct:       int(minOverlap * 100),
		DistinctRuns:        len(commonRuns),
		CommonRunIDs:        outputRuns,
		CommonRunsTruncated: commonRunsTruncated,
		CommonEV2:           slices.Sorted(maps.Keys(ev2Set)),
		CommonRegions:       slices.Sorted(maps.Keys(regionSet)),
		Onset:               onsetStr,
	}
}

func setIntersectionCount(a, b map[int64]bool) int {
	if len(a) > len(b) {
		a, b = b, a
	}
	count := 0
	for k := range a {
		if b[k] {
			count++
		}
	}
	return count
}
