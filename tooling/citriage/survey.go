package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	maxRunsFetch = 1000

	lowSampleThreshold      = 3
	boundaryDetectionWindow = 24 * time.Hour
	isolatedFailureCeiling  = 3
	moderateFailureCeiling  = 15
	neighborRunsLimit       = 100
)

// rankedFailure pairs a RecentFailure with computed metadata: regular-run
// hit count, best run to link to, and affected regions.
type rankedFailure struct {
	failure     RecentFailure
	regularHits int
	bestRunID   int64
	bestRunURL  string
	regions     []string
}

// runSurvey parses CLI flags and dispatches to the appropriate survey mode:
// single environment or all environments.
func runSurvey(args []string) error {
	fs := flag.NewFlagSet("survey", flag.ExitOnError)
	env := fs.String("env", "all", "environment: int, stg, prod, dev, all")
	days := fs.Int("days", 7, "lookback days")
	job := fs.String("job", "", "override job name filter")
	test := fs.String("test", "", "filter failing tests by name substring")
	cofailure := fs.Float64("cofailure", 0.8, "co-failure overlap threshold (0=disable)")
	_ = fs.Int("blast", 0, "deprecated, ignored")
	fs.Parse(args)

	s := newSippy()
	if *env == "all" {
		return surveyAll(s, *days, *job, *test, *cofailure)
	}
	return surveyEnv(s, *env, *days, *job, *test, *cofailure)
}

// surveyData holds all fetched and computed data for a single environment survey.
type surveyData struct {
	release              string
	runs                 []JobRun
	ranked               []rankedFailure
	runMap               map[int64]JobRun
	requestedDays        int
	nightlyRunsExcluded  int
	runCountCapped       bool
}

// --- JSON output types — the contract between tooling and LLM ---

type surveyJSON struct {
	Env             string               `json:"env"`
	Release         string               `json:"release"`
	Status          statusJSON           `json:"status"`
	DataWindow      *dataWindowJSON      `json:"data_window,omitempty"`
	DailyRates      []dailyRateJSON      `json:"daily_rates"`
	EV2Coverage     ev2CoverageJSON      `json:"ev2_coverage"`
	EV2HashRates    []ev2HashRateJSON    `json:"ev2_hash_rates,omitempty"`
	FailureScaleDist *failureScaleDistJSON `json:"failure_scale_dist,omitempty"`
	RegionRates     []regionRateJSON     `json:"region_rates,omitempty"`
	Runs            []runJSON            `json:"runs"`
	Failures        []failureJSON        `json:"failures"`
	CoFailureGroups []coFailureGroupJSON `json:"co_failure_groups,omitempty"`
	CoFailureStats  *coFailureStatsJSON  `json:"co_failure_stats,omitempty"`
}

type dataWindowJSON struct {
	RequestedDays        int    `json:"requested_days"`
	ActualDays           int    `json:"actual_days"`
	OldestRun            string `json:"oldest_run,omitempty"`
	NewestRun            string `json:"newest_run,omitempty"`
	Truncated            bool   `json:"truncated"`
	NightlyRunsExcluded  int    `json:"nightly_runs_excluded,omitempty"`
	RunCountCapped       bool   `json:"run_count_capped,omitempty"`
	Empty                bool   `json:"empty,omitempty"`
	EmptyReason          string `json:"empty_reason,omitempty"`
}

type statusJSON struct {
	Streak        int      `json:"streak"`
	CurrentGreen  bool     `json:"current_green"`
	StreakRegions []string `json:"streak_regions,omitempty"`
	PassRate      float64  `json:"pass_rate"`
	TotalRuns     int      `json:"total_runs"`
}

type dailyRateJSON struct {
	Date  string `json:"date"`
	Pass  int    `json:"pass"`
	Total int    `json:"total"`
}

type regionRateJSON struct {
	Region    string  `json:"region"`
	Pass      int     `json:"pass"`
	Total     int     `json:"total"`
	PassRate  float64 `json:"pass_rate"`
	LowSample bool   `json:"low_sample,omitempty"`
}

type ev2CoverageJSON struct {
	WithEV2 int `json:"with_ev2"`
	Total   int `json:"total"`
}

type runJSON struct {
	ID                   int64    `json:"id"`
	Timestamp            int64    `json:"timestamp"`
	Result               string   `json:"overall_result"`
	RealFailures         int      `json:"real_failures"`
	FailedTests          []string `json:"failed_tests,omitempty"`
	FailedTestsTruncated bool     `json:"failed_tests_truncated,omitempty"`
	EV2Hash              string   `json:"ev2_hash,omitempty"`
	Region               string   `json:"region,omitempty"`
	Cluster              string   `json:"cluster,omitempty"`
	PullNumber           int      `json:"pull_number,omitempty"`
	URL                  string   `json:"url"`
}

type failureJSON struct {
	TestName          string           `json:"test_name"`
	FailureCount      int              `json:"failure_count"`
	RegularHits       int              `json:"regular_hits"`
	FirstFailure      string           `json:"first_failure"`
	LastFailure       string           `json:"last_failure"`
	LastPass          string           `json:"last_pass,omitempty"`
	Regions           []string         `json:"regions,omitempty"`
	AtWindowBoundary  bool             `json:"at_window_boundary"`
	Chronicity        string           `json:"chronicity,omitempty"`
	BestRunID         int64            `json:"best_run_id"`
	BestRunURL        string           `json:"best_run_url,omitempty"`
	ErrorGroups       []errorGroupJSON `json:"error_groups"`
	InnermostCause    string           `json:"innermost_cause,omitempty"`
	DailyHits         []dailyHitJSON   `json:"daily_hits,omitempty"`
	TotalRuns         int              `json:"total_runs"`
}

type errorGroupJSON struct {
	Error           string `json:"error"`
	ExtractedErrors string `json:"extracted_errors,omitempty"`
	Count           int    `json:"count"`
}

type dailyHitJSON struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type ev2HashRateJSON struct {
	Hash     string  `json:"hash"`
	Pass     int     `json:"pass"`
	Fail     int     `json:"fail"`
	Total    int     `json:"total"`
	PassRate float64 `json:"pass_rate"`
	IsCron   bool    `json:"is_cron,omitempty"`
}

type failureScaleDistJSON struct {
	None      int `json:"none"`
	Isolated  int `json:"isolated"`
	Moderate  int `json:"moderate"`
	Cascade   int `json:"cascade"`
}

type crossEnvEntryJSON struct {
	Env   string `json:"env"`
	Hits  int    `json:"hits"`
	RunID int64  `json:"run_id"`
}

type crossEnvJSON struct {
	TestName     string              `json:"test_name"`
	EnvCount     int                 `json:"env_count"`
	Environments []crossEnvEntryJSON `json:"environments"`
}

type surveyAllJSON struct {
	Environments     []surveyJSON   `json:"environments"`
	CrossEnvFailures []crossEnvJSON `json:"cross_env_failures"`
}

// --- Data fetching ---

// fetchSurveyData fetches fleet health, runs, and failures for a single
// environment, enriches failures with artifact data when needed, and ranks
// them by frequency.
func fetchSurveyData(s *sippy, env string, days int, jobPat, testPat string) (*surveyData, error) {
	release, ok := envRelease[env]
	if !ok {
		return nil, fmt.Errorf("unknown env %q (use int, stg, prod, dev, all)", env)
	}

	jobFilter := defaultJobFilter(env)
	if jobPat != "" {
		jobFilter = jobPat
	}
	runs, err := s.listRuns(release, maxRunsFetch, jobFilter)
	if err != nil {
		return nil, err
	}
	runCountCapped := len(runs) >= maxRunsFetch
	nightlyExcluded := 0
	if jobPat == "" {
		runs, nightlyExcluded = filterNightlyRuns(runs)
	}
	runs = filterRunsByDate(runs, time.Now().Add(-time.Duration(days)*24*time.Hour))

	period := fmt.Sprintf("%dh", days*24)
	failures, err := s.recentFailures(release, period)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sippy recent_failures unavailable, using run data: %v\n", err)
	}
	if len(failures) == 0 {
		failures = failuresFromRuns(runs)
	}
	enrichMissingErrors(failures, runs)
	extractPipelineStepErrors(failures)

	runMap, regularRunIDs := buildRunLookup(runs)
	ranked := rankFailures(failures, runMap, regularRunIDs, testPat)

	return &surveyData{
		release:             release,
		runs:                runs,
		ranked:              ranked,
		runMap:              runMap,
		requestedDays:       days,
		nightlyRunsExcluded: nightlyExcluded,
		runCountCapped:      runCountCapped,
	}, nil
}

// --- JSON building (structured data, no rendering) ---

// buildSurveyJSON converts surveyData into a structured JSON representation
// with no truncation, no sorting, and raw numeric values.
func buildSurveyJSON(env string, data *surveyData, cofailureThreshold float64) surveyJSON {
	result := surveyJSON{
		Env:     env,
		Release: data.release,
	}

	if len(data.runs) > 0 {
		streak, green := computeStreak(data.runs)
		regions := collectRegions(data.runs[:min(streak, len(data.runs))])
		passed := 0
		for _, r := range data.runs {
			if realFailureCount(r) == 0 {
				passed++
			}
		}
		result.Status = statusJSON{
			Streak:        streak,
			CurrentGreen:  green,
			StreakRegions: slices.Sorted(maps.Keys(regions)),
			PassRate:      100.0 * float64(passed) / float64(len(data.runs)),
			TotalRuns:     len(data.runs),
		}
	}

	result.DataWindow = buildDataWindow(data.runs, data.requestedDays, data.nightlyRunsExcluded, data.runCountCapped)
	result.DailyRates = buildDailyRates(data.runs)
	result.EV2Coverage = buildEV2Coverage(data.runs)
	result.EV2HashRates = buildEV2HashRates(data.runs)
	result.FailureScaleDist = buildFailureScaleDist(data.runs)
	result.RegionRates = buildRegionRates(data.runs)
	result.Runs = buildRunsJSON(data.runs)
	var windowStart time.Time
	if len(data.runs) > 0 {
		windowStart = time.UnixMilli(data.runs[len(data.runs)-1].Timestamp).UTC()
	}
	result.Failures = buildFailuresJSON(data.ranked, data.runMap, len(data.runs), windowStart)
	result.CoFailureGroups, result.CoFailureStats = buildCoFailureGroups(data.runs, cofailureThreshold)

	return result
}

func buildDataWindow(runs []JobRun, requestedDays, nightlyExcluded int, runCountCapped bool) *dataWindowJSON {
	if requestedDays == 0 {
		return nil
	}
	if len(runs) == 0 {
		return &dataWindowJSON{
			RequestedDays:       requestedDays,
			NightlyRunsExcluded: nightlyExcluded,
			RunCountCapped:      runCountCapped,
			Empty:               true,
			EmptyReason:         "no runs matched the job filter within the requested time window",
		}
	}
	oldest := runs[len(runs)-1].Timestamp
	newest := runs[0].Timestamp
	actualSpan := time.UnixMilli(newest).Sub(time.UnixMilli(oldest))
	actualDays := int(actualSpan.Hours()/24) + 1
	truncated := actualDays < requestedDays-1 || runCountCapped
	if truncated {
		fmt.Fprintf(os.Stderr, "warning: requested %d days but data only spans %d days (%s to %s)\n",
			requestedDays, actualDays,
			time.UnixMilli(oldest).UTC().Format("2006-01-02"),
			time.UnixMilli(newest).UTC().Format("2006-01-02"))
	}
	return &dataWindowJSON{
		RequestedDays:       requestedDays,
		ActualDays:          actualDays,
		OldestRun:           time.UnixMilli(oldest).UTC().Format(time.RFC3339),
		NewestRun:           time.UnixMilli(newest).UTC().Format(time.RFC3339),
		Truncated:           truncated,
		NightlyRunsExcluded: nightlyExcluded,
		RunCountCapped:      runCountCapped,
	}
}

func buildDailyRates(runs []JobRun) []dailyRateJSON {
	type dayBucket struct{ pass, total int }
	buckets := map[string]*dayBucket{}
	for _, r := range runs {
		day := time.UnixMilli(r.Timestamp).UTC().Format("2006-01-02")
		b, ok := buckets[day]
		if !ok {
			b = &dayBucket{}
			buckets[day] = b
		}
		b.total++
		if realFailureCount(r) == 0 {
			b.pass++
		}
	}
	days := slices.Sorted(maps.Keys(buckets))
	result := make([]dailyRateJSON, len(days))
	for i, day := range days {
		b := buckets[day]
		result[i] = dailyRateJSON{Date: day, Pass: b.pass, Total: b.total}
	}
	return result
}

func buildEV2Coverage(runs []JobRun) ev2CoverageJSON {
	withEV2 := 0
	for _, r := range runs {
		if ev2Hash(r) != "" {
			withEV2++
		}
	}
	return ev2CoverageJSON{WithEV2: withEV2, Total: len(runs)}
}

func buildEV2HashRates(runs []JobRun) []ev2HashRateJSON {
	type bucket struct{ pass, fail int }
	buckets := map[string]*bucket{}
	for _, r := range runs {
		hash := ev2Hash(r)
		if hash == "" {
			hash = "NO_HASH"
		}
		b, ok := buckets[hash]
		if !ok {
			b = &bucket{}
			buckets[hash] = b
		}
		if realFailureCount(r) == 0 {
			b.pass++
		} else {
			b.fail++
		}
	}
	hashes := slices.Sorted(maps.Keys(buckets))
	result := make([]ev2HashRateJSON, 0, len(hashes))
	for _, hash := range hashes {
		b := buckets[hash]
		total := b.pass + b.fail
		result = append(result, ev2HashRateJSON{
			Hash:     hash,
			Pass:     b.pass,
			Fail:     b.fail,
			Total:    total,
			PassRate: 100.0 * float64(b.pass) / float64(total),
			IsCron:   hash == "NO_HASH",
		})
	}
	slices.SortFunc(result, func(a, b ev2HashRateJSON) int {
		return cmp.Compare(b.Total, a.Total)
	})
	return result
}

func buildFailureScaleDist(runs []JobRun) *failureScaleDistJSON {
	dist := &failureScaleDistJSON{}
	for _, r := range runs {
		fc := realFailureCount(r)
		switch {
		case fc == 0:
			dist.None++
		case fc <= isolatedFailureCeiling:
			dist.Isolated++
		case fc <= moderateFailureCeiling:
			dist.Moderate++
		default:
			dist.Cascade++
		}
	}
	return dist
}

func buildRegionRates(runs []JobRun) []regionRateJSON {
	type bucket struct{ pass, total int }
	buckets := map[string]*bucket{}
	for _, r := range runs {
		region := ev2Region(r)
		if region == "" {
			continue
		}
		b, ok := buckets[region]
		if !ok {
			b = &bucket{}
			buckets[region] = b
		}
		b.total++
		if realFailureCount(r) == 0 {
			b.pass++
		}
	}
	if len(buckets) == 0 {
		return nil
	}
	regions := slices.Sorted(maps.Keys(buckets))
	result := make([]regionRateJSON, len(regions))
	for i, region := range regions {
		b := buckets[region]
		result[i] = regionRateJSON{
			Region:    region,
			Pass:      b.pass,
			Total:     b.total,
			PassRate:  100.0 * float64(b.pass) / float64(b.total),
			LowSample: b.total < lowSampleThreshold,
		}
	}
	return result
}

func buildRunsJSON(runs []JobRun) []runJSON {
	const maxFailedTestsPerRun = 5
	result := make([]runJSON, len(runs))
	for i, r := range runs {
		var ev2 string
		if r.Annotations != nil {
			ev2 = r.Annotations[ev2HashAnnotation]
		}
		var failedTests []string
		for _, name := range r.FailedTestNames {
			if !isSyntheticTest(name) {
				failedTests = append(failedTests, name)
			}
		}
		truncated := len(failedTests) > maxFailedTestsPerRun
		if truncated {
			failedTests = failedTests[:maxFailedTestsPerRun]
		}
		result[i] = runJSON{
			ID:                   r.ID,
			Timestamp:            r.Timestamp,
			Result:               r.OverallResult,
			RealFailures:         realFailureCount(r),
			FailedTests:          failedTests,
			FailedTestsTruncated: truncated,
			EV2Hash:              ev2,
			Region:               ev2Region(r),
			Cluster:              r.Cluster,
			PullNumber:           extractPullNumber(r.URL),
			URL:                  r.URL,
		}
	}
	return result
}

func buildFailuresJSON(ranked []rankedFailure, runMap map[int64]JobRun, totalRuns int, windowStart time.Time) []failureJSON {
	result := make([]failureJSON, len(ranked))
	for i, rf := range ranked {
		f := rf.failure
		atBoundary := false
		if !windowStart.IsZero() && f.FirstFailure != "" {
			if t, err := time.Parse(time.RFC3339, f.FirstFailure); err == nil {
				atBoundary = t.Sub(windowStart) < boundaryDetectionWindow
			}
		}
		chronicity := ""
		if atBoundary {
			chronicity = "at_boundary"
		} else if f.FirstFailure != "" {
			chronicity = "within_window"
		}
		if f.LastPass != "" && f.LastFailure != "" {
			if lp, err1 := time.Parse(time.RFC3339, f.LastPass); err1 == nil {
				if lf, err2 := time.Parse(time.RFC3339, f.LastFailure); err2 == nil {
					if lp.After(lf) || lp.Equal(lf) {
						chronicity = "intermittent"
					}
				}
			}
		}
		errorGroups := groupErrorOutputsFull(f)
		var innermostCause string
		if len(errorGroups) > 0 {
			dominant := errorGroups[0]
			for _, eg := range errorGroups[1:] {
				if eg.Count > dominant.Count {
					dominant = eg
				}
			}
			innermostCause, _ = extractInnermostCause(dominant.Error)
		}
		result[i] = failureJSON{
			TestName:         f.TestName,
			FailureCount:     f.FailureCount,
			RegularHits:      rf.regularHits,
			FirstFailure:     f.FirstFailure,
			LastFailure:      f.LastFailure,
			LastPass:         f.LastPass,
			Regions:          rf.regions,
			AtWindowBoundary: atBoundary,
			Chronicity:       chronicity,
			BestRunID:        rf.bestRunID,
			BestRunURL:       rf.bestRunURL,
			ErrorGroups:      errorGroups,
			InnermostCause:   innermostCause,
			DailyHits:        buildDailyHits(f, runMap),
			TotalRuns:        totalRuns,
		}
	}
	return result
}

func buildDailyHits(f RecentFailure, runMap map[int64]JobRun) []dailyHitJSON {
	if len(runMap) == 0 || len(f.Outputs) < 2 {
		return nil
	}
	dayCounts := map[string]int{}
	for _, out := range f.Outputs {
		r, ok := runMap[out.RunID]
		if !ok {
			continue
		}
		dayCounts[time.UnixMilli(r.Timestamp).UTC().Format("2006-01-02")]++
	}
	allDays := slices.Sorted(maps.Keys(dayCounts))
	result := make([]dailyHitJSON, len(allDays))
	for i, day := range allDays {
		result[i] = dailyHitJSON{Date: day, Count: dayCounts[day]}
	}
	return result
}

// groupErrorOutputsFull returns error groups with no truncation (for JSON mode).
func groupErrorOutputsFull(f RecentFailure) []errorGroupJSON {
	if len(f.Outputs) == 0 {
		return nil
	}

	type bucket struct {
		full      string
		extracted string
		count     int
	}
	buckets := map[string]*bucket{}
	var order []string

	for _, out := range f.Outputs {
		if out.Output == "" {
			continue
		}
		key := normalizeForSimilarity(out.Output)

		if b, ok := buckets[key]; ok {
			b.count++
		} else {
			buckets[key] = &bucket{full: out.Output, extracted: out.ExtractedErrors, count: 1}
			order = append(order, key)
		}
	}

	groups := make([]errorGroupJSON, 0, len(buckets))
	for _, key := range order {
		b := buckets[key]
		groups = append(groups, errorGroupJSON{Error: b.full, ExtractedErrors: b.extracted, Count: b.count})
	}
	return groups
}

// --- Dispatch functions ---

func surveyEnv(s *sippy, env string, days int, jobPat, testPat string, cofailureThreshold float64) error {
	data, err := fetchSurveyData(s, env, days, jobPat, testPat)
	if err != nil {
		return err
	}
	sj := buildSurveyJSON(env, data, cofailureThreshold)
	return json.NewEncoder(os.Stdout).Encode(sj)
}

func surveyAll(s *sippy, days int, jobPat, testPat string, cofailureThreshold float64) error {
	envs := []string{"int", "stg", "prod"}
	type envResult struct {
		env    string
		survey surveyJSON
		ranked []rankedFailure
		err    error
	}
	ch := make(chan envResult, len(envs))
	for _, e := range envs {
		go func(env string) {
			data, err := fetchSurveyData(s, env, days, jobPat, testPat)
			if err != nil {
				ch <- envResult{env: env, err: err}
				return
			}
			sj := buildSurveyJSON(env, data, cofailureThreshold)
			ch <- envResult{env: env, survey: sj, ranked: data.ranked}
		}(e)
	}
	results := make(map[string]envResult)
	for range envs {
		r := <-ch
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", r.env, r.err)
			continue
		}
		results[r.env] = r
	}

	var envResults []surveyJSON
	testEnvEntries := map[string][]crossEnvEntryJSON{}
	for _, e := range envs {
		r, ok := results[e]
		if !ok {
			continue
		}
		envResults = append(envResults, r.survey)
		for _, rf := range r.ranked {
			testEnvEntries[rf.failure.TestName] = append(testEnvEntries[rf.failure.TestName], crossEnvEntryJSON{
				Env:   e,
				Hits:  rf.failure.FailureCount,
				RunID: rf.bestRunID,
			})
		}
	}

	var crossEnv []crossEnvJSON
	for testName, entries := range testEnvEntries {
		if len(entries) < 2 {
			continue
		}
		crossEnv = append(crossEnv, crossEnvJSON{
			TestName:     testName,
			EnvCount:     len(entries),
			Environments: entries,
		})
	}
	slices.SortFunc(crossEnv, func(a, b crossEnvJSON) int {
		if c := cmp.Compare(b.EnvCount, a.EnvCount); c != 0 {
			return c
		}
		return cmp.Compare(a.TestName, b.TestName)
	})

	result := surveyAllJSON{
		Environments:     envResults,
		CrossEnvFailures: crossEnv,
	}

	return json.NewEncoder(os.Stdout).Encode(result)
}

// --- Data helpers ---

// filterRunsByDate removes runs older than the cutoff time.
func filterRunsByDate(runs []JobRun, cutoff time.Time) []JobRun {
	cutoffMillis := cutoff.UnixMilli()
	var filtered []JobRun
	for _, r := range runs {
		if r.Timestamp >= cutoffMillis {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// filterNightlyRuns removes ocp-nightly runs from the list, returning
// the filtered list and the count of excluded runs.
func filterNightlyRuns(runs []JobRun) ([]JobRun, int) {
	var filtered []JobRun
	excluded := 0
	for _, r := range runs {
		if strings.Contains(r.Job, "ocp-nightly") {
			excluded++
		} else {
			filtered = append(filtered, r)
		}
	}
	return filtered, excluded
}

// computeStreak counts consecutive runs with the same pass/fail status
// from the most recent run.
func computeStreak(runs []JobRun) (streak int, currentGreen bool) {
	currentGreen = realFailureCount(runs[0]) == 0
	for _, r := range runs {
		if (realFailureCount(r) == 0) == currentGreen {
			streak++
		} else {
			break
		}
	}
	return
}

// collectRegions extracts unique region names from a slice of runs.
func collectRegions(runs []JobRun) map[string]bool {
	regions := map[string]bool{}
	for _, r := range runs {
		if reg := ev2Region(r); reg != "" {
			regions[reg] = true
		}
	}
	return regions
}

// buildRunLookup creates maps for quick run lookup by ID and for identifying
// which run IDs belong to regular (non-nightly) runs.
func buildRunLookup(runs []JobRun) (map[int64]JobRun, map[int64]bool) {
	runMap := map[int64]JobRun{}
	regularRunIDs := map[int64]bool{}
	for _, r := range runs {
		runMap[r.ID] = r
		regularRunIDs[r.ID] = true
	}
	return runMap, regularRunIDs
}

// enrichFailure computes per-failure metadata: regular-run hit count,
// best run link, and affected regions.
func enrichFailure(f RecentFailure, runMap map[int64]JobRun, regularRunIDs map[int64]bool) rankedFailure {
	rf := rankedFailure{failure: f}
	regionSet := map[string]bool{}
	for _, out := range f.Outputs {
		if regularRunIDs[out.RunID] {
			rf.regularHits++
			if rf.bestRunID == 0 {
				rf.bestRunID = out.RunID
				if r, ok := runMap[out.RunID]; ok {
					rf.bestRunURL = r.URL
				}
			}
		}
		if r, ok := runMap[out.RunID]; ok {
			if reg := ev2Region(r); reg != "" {
				regionSet[reg] = true
			}
		}
	}
	rf.regions = slices.Sorted(maps.Keys(regionSet))
	if rf.bestRunID == 0 && len(f.Outputs) > 0 {
		rf.bestRunID = f.Outputs[0].RunID
	}
	return rf
}

// rankFailures filters, enriches, and sorts failures by frequency.
func rankFailures(failures []RecentFailure, runMap map[int64]JobRun, regularRunIDs map[int64]bool, testPat string) []rankedFailure {
	var ranked []rankedFailure
	for _, f := range failures {
		if isSyntheticTest(f.TestName) {
			continue
		}
		if testPat != "" && !strings.Contains(strings.ToLower(f.TestName), strings.ToLower(testPat)) {
			continue
		}
		rf := enrichFailure(f, runMap, regularRunIDs)
		ranked = append(ranked, rf)
	}

	slices.SortFunc(ranked, func(a, b rankedFailure) int {
		if a.failure.FailureCount != b.failure.FailureCount {
			return cmp.Compare(b.failure.FailureCount, a.failure.FailureCount)
		}
		return cmp.Compare(b.regularHits, a.regularHits)
	})
	return ranked
}

// --- Utility functions ---

func ev2Hash(r JobRun) string {
	if r.Annotations == nil {
		return ""
	}
	return r.Annotations[ev2HashAnnotation]
}

func ev2Region(r JobRun) string {
	if r.Annotations == nil {
		return ""
	}
	return r.Annotations[ev2RegionAnnotation]
}

func isSyntheticTest(name string) bool {
	return strings.HasPrefix(name, syntheticTestPrefix) || name == syntheticTestTimeout
}

// realFailureCount returns the number of non-synthetic test failures for a run.
func realFailureCount(r JobRun) int {
	if len(r.FailedTestNames) > 0 {
		n := 0
		for _, name := range r.FailedTestNames {
			if !isSyntheticTest(name) {
				n++
			}
		}
		return n
	}
	return r.TestFailures
}

// --- Pipeline step error extraction ---

// logEntryStartRe matches the start of a templatize structured log entry.
var logEntryStartRe = regexp.MustCompile(`time=\d{4}-\d{2}-\d{2}T`)

// extractPipelineStepErrors extracts ERROR/FATAL-level entries from pipeline
// step log output into ExtractedErrors, preserving the original Output.
func extractPipelineStepErrors(failures []RecentFailure) {
	for i := range failures {
		if !strings.HasPrefix(failures[i].TestName, "Run pipeline step ") {
			continue
		}
		for j := range failures[i].Outputs {
			if failures[i].Outputs[j].Output == "" {
				continue
			}
			if extracted := extractStepError(failures[i].Outputs[j].Output); extracted != "" {
				failures[i].Outputs[j].ExtractedErrors = extracted
			}
		}
	}
}

// extractStepError finds ERROR/FATAL level entries in templatize structured
// log output. Returns the error entries joined, or empty if none found
// (caller keeps original output as fallback).
func extractStepError(output string) string {
	locs := logEntryStartRe.FindAllStringIndex(output, -1)
	if len(locs) == 0 {
		return ""
	}
	var errors []string
	for i, loc := range locs {
		end := len(output)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		entry := strings.TrimSpace(output[loc[0]:end])
		lower := strings.ToLower(entry)
		if strings.Contains(lower, "level=error") || strings.Contains(lower, "level=fatal") {
			errors = append(errors, entry)
		}
	}
	if len(errors) == 0 {
		return ""
	}
	return strings.Join(errors, "\n")
}


func normalizeForSimilarity(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func stripGoPointers(s string) string {
	return s
}

// enrichMissingErrors fetches extension_test_result.json from failing runs to fill
// in error text when Sippy didn't provide it (common for presubmit).
func enrichMissingErrors(failures []RecentFailure, runs []JobRun) {
	needsEnrich := false
	for _, f := range failures {
		for _, out := range f.Outputs {
			if out.Output == "" {
				needsEnrich = true
				break
			}
		}
		if needsEnrich {
			break
		}
	}
	if !needsEnrich {
		return
	}

	// Pick top failing runs (most test failures) to fetch error text from
	var targets []JobRun
	for _, r := range runs {
		if realFailureCount(r) > 0 {
			targets = append(targets, r)
		}
	}
	if len(targets) > 20 {
		targets = targets[:20]
	}
	if len(targets) == 0 {
		return
	}

	store := newGCS()
	type fetchResult struct {
		runID  int64
		errors map[string]string
	}
	ch := make(chan fetchResult, len(targets))
	for _, r := range targets {
		base := gcsBase(r.URL)
		if base == "" {
			ch <- fetchResult{r.ID, nil}
			continue
		}
		go func(id int64, b, job string) {
			step, container := stepContainer(job)
			prefix := b + "artifacts/" + step + "/" + container + "/artifacts/"
			_, files, err := store.listDir(prefix)
			if err != nil {
				ch <- fetchResult{id, nil}
				return
			}
			f := findExtensionResultFile(files)
			if f == "" {
				ch <- fetchResult{id, nil}
				return
			}
			data, err := store.fetch(gcsDownload + f)
			if err != nil {
				ch <- fetchResult{id, nil}
				return
			}
			var tests []extensionTestResult
			if json.Unmarshal(data, &tests) != nil {
				ch <- fetchResult{id, nil}
				return
			}
			m := map[string]string{}
			for _, t := range tests {
				if t.Error != "" {
					m[t.Name] = t.Error
				}
			}
			ch <- fetchResult{id, m}
		}(r.ID, base, r.Job)
	}

	errorsByTest := map[string]string{}
	for range len(targets) {
		r := <-ch
		for name, errText := range r.errors {
			if _, exists := errorsByTest[name]; !exists {
				errorsByTest[name] = errText
			}
		}
	}

	for i, f := range failures {
		errText, ok := errorsByTest[f.TestName]
		if !ok {
			continue
		}
		for j, out := range f.Outputs {
			if out.Output == "" {
				failures[i].Outputs[j].Output = errText
			}
		}
	}
}

// failuresFromRuns synthesizes RecentFailure data from the FailedTestNames
// field on each run, used when Sippy recent_failures API is unavailable.
func failuresFromRuns(runs []JobRun) []RecentFailure {
	type testAcc struct {
		count   int
		first   string
		last    string
		outputs []FailureOutput
	}
	tests := map[string]*testAcc{}
	for _, r := range runs {
		ts := time.UnixMilli(r.Timestamp).UTC().Format(time.RFC3339)
		for _, name := range r.FailedTestNames {
			if isSyntheticTest(name) {
				continue
			}
			a, ok := tests[name]
			if !ok {
				a = &testAcc{first: ts, last: ts}
				tests[name] = a
			}
			a.count++
			if ts < a.first {
				a.first = ts
			}
			if ts > a.last {
				a.last = ts
			}
			a.outputs = append(a.outputs, FailureOutput{RunID: r.ID})
		}
	}

	result := make([]RecentFailure, 0, len(tests))
	for name, a := range tests {
		result = append(result, RecentFailure{
			TestName:     name,
			FailureCount: a.count,
			FirstFailure: a.first,
			LastFailure:  a.last,
			Outputs:      a.outputs,
		})
	}
	return result
}
