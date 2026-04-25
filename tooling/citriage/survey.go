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
	maxRunsFetch       = 1000
	maxEnrichTargets   = 25
	maxDurationFetches = 20

	minHexAddrLen        = 10
	maxTypeAnnotationLen = 60

	lowSampleThreshold      = 3     // region rates below this run count are flagged
	maxDurationDays         = 7     // recent duration history window
	maxDurationPerDay       = 86400 // filter out durations above 1 day (bogus data)
	prOutlierRunThreshold   = 5     // PRs with more runs than this can skew stats
	boundaryDetectionWindow = 24 * time.Hour
	isolatedFailureCeiling  = 3  // <=N failures = "isolated"
	moderateFailureCeiling  = 15 // <=N failures = "moderate", above = "cascade"
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
	blast := fs.Int("blast", 10, "exclude runs with more than N failures from co-failure analysis (0=disable)")
	fs.Parse(args)

	s := newSippy()
	if *env == "all" {
		return surveyAll(s, *days, *job, *test, *cofailure, *blast)
	}
	return surveyEnv(s, *env, *days, *job, *test, *cofailure, *blast)
}

// surveyData holds all fetched and computed data for a single environment survey.
type surveyData struct {
	release       string
	runs          []JobRun
	ranked        []rankedFailure
	runMap        map[int64]JobRun
	requestedDays int
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
	PRStats         *prStatsJSON         `json:"pr_stats,omitempty"`
}

type dataWindowJSON struct {
	RequestedDays int    `json:"requested_days"`
	ActualDays    int    `json:"actual_days"`
	OldestRun     string `json:"oldest_run,omitempty"`
	NewestRun     string `json:"newest_run,omitempty"`
	Truncated     bool   `json:"truncated"`
	Empty         bool   `json:"empty,omitempty"`
	EmptyReason   string `json:"empty_reason,omitempty"`
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
	ID           int64    `json:"id"`
	Timestamp    int64    `json:"timestamp"`
	Result       string   `json:"overall_result"`
	RealFailures int      `json:"real_failures"`
	FailedTests  []string `json:"failed_tests,omitempty"`
	EV2Hash      string   `json:"ev2_hash,omitempty"`
	Region       string   `json:"region,omitempty"`
	Cluster      string   `json:"cluster,omitempty"`
	PullNumber   int      `json:"pull_number,omitempty"`
	URL          string   `json:"url"`
}

type failureJSON struct {
	TestName         string           `json:"test_name"`
	FailureCount     int              `json:"failure_count"`
	RegularHits      int              `json:"regular_hits"`
	FirstFailure     string           `json:"first_failure"`
	LastFailure      string           `json:"last_failure"`
	LastPass         string           `json:"last_pass,omitempty"`
	Regions          []string         `json:"regions,omitempty"`
	AtWindowBoundary bool             `json:"at_window_boundary"`
	Chronicity       string           `json:"chronicity,omitempty"`
	BestRunID        int64            `json:"best_run_id"`
	BestRunURL       string           `json:"best_run_url,omitempty"`
	ErrorGroups      []errorGroupJSON `json:"error_groups"`
	NormalizedError  string           `json:"normalized_error,omitempty"`
	DailyHits        []dailyHitJSON   `json:"daily_hits,omitempty"`
	Durations        []float64        `json:"durations,omitempty"`
	TotalRuns        int              `json:"total_runs"`
}

type errorGroupJSON struct {
	Error string `json:"error"`
	Count int    `json:"count"`
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

type prEntryJSON struct {
	Number      int      `json:"number"`
	Runs        int      `json:"runs"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	PassRate    float64  `json:"pass_rate"`
	FailedTests []string `json:"failed_tests"`
	Outlier     bool     `json:"outlier,omitempty"`
	URL         string   `json:"url"`
}

type prStatsJSON struct {
	DistinctPRs        int           `json:"distinct_prs"`
	PRWeightedPassRate float64       `json:"pr_weighted_pass_rate"`
	RunWeightedNote    string        `json:"run_weighted_note,omitempty"`
	PRs                []prEntryJSON `json:"prs"`
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
	if jobPat == "" {
		runs = filterNightlyRuns(runs)
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
	if len(failures) > 0 && needsErrorEnrichment(failures) {
		enrichFailuresWithArtifacts(failures, runs)
	}
	extractPipelineStepErrors(failures)

	runMap, regularRunIDs := buildRunLookup(runs)
	ranked := rankFailures(failures, runMap, regularRunIDs, testPat, jobPat)

	return &surveyData{
		release:       release,
		runs:          runs,
		ranked:        ranked,
		runMap:        runMap,
		requestedDays: days,
	}, nil
}

// --- JSON building (structured data, no rendering) ---

// buildSurveyJSON converts surveyData into a structured JSON representation
// with no truncation, no sorting, and raw numeric values.
func buildSurveyJSON(env string, data *surveyData, cofailureThreshold float64, blastRadius int, s ...*sippy) surveyJSON {
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

	result.DataWindow = buildDataWindow(data.runs, data.requestedDays)
	result.DailyRates = buildDailyRates(data.runs)
	result.EV2Coverage = buildEV2Coverage(data.runs)
	result.EV2HashRates = buildEV2HashRates(data.runs)
	result.FailureScaleDist = buildFailureScaleDist(data.runs)
	result.RegionRates = buildRegionRates(data.runs)
	result.Runs = buildRunsJSON(data.runs)
	var sc *sippy
	if len(s) > 0 {
		sc = s[0]
	}
	var windowStart time.Time
	if len(data.runs) > 0 {
		windowStart = time.UnixMilli(data.runs[len(data.runs)-1].Timestamp).UTC()
	}
	result.Failures = buildFailuresJSON(data.ranked, data.runMap, len(data.runs), sc, data.release, windowStart)
	result.CoFailureGroups, result.CoFailureStats = buildCoFailureGroups(data.runs, cofailureThreshold, blastRadius)
	result.PRStats = buildPRStats(data.runs)

	return result
}

func buildDataWindow(runs []JobRun, requestedDays int) *dataWindowJSON {
	if requestedDays == 0 {
		return nil
	}
	if len(runs) == 0 {
		return &dataWindowJSON{
			RequestedDays: requestedDays,
			Empty:         true,
			EmptyReason:   "no runs matched the job filter within the requested time window",
		}
	}
	oldest := runs[len(runs)-1].Timestamp
	newest := runs[0].Timestamp
	actualSpan := time.UnixMilli(newest).Sub(time.UnixMilli(oldest))
	actualDays := int(actualSpan.Hours()/24) + 1
	truncated := actualDays < requestedDays-1
	if truncated {
		fmt.Fprintf(os.Stderr, "warning: requested %d days but data only spans %d days (%s to %s)\n",
			requestedDays, actualDays,
			time.UnixMilli(oldest).UTC().Format("2006-01-02"),
			time.UnixMilli(newest).UTC().Format("2006-01-02"))
	}
	return &dataWindowJSON{
		RequestedDays: requestedDays,
		ActualDays:    actualDays,
		OldestRun:     time.UnixMilli(oldest).UTC().Format(time.RFC3339),
		NewestRun:     time.UnixMilli(newest).UTC().Format(time.RFC3339),
		Truncated:     truncated,
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
		result[i] = runJSON{
			ID:           r.ID,
			Timestamp:    r.Timestamp,
			Result:       r.OverallResult,
			RealFailures: realFailureCount(r),
			FailedTests:  failedTests,
			EV2Hash:      ev2,
			Region:       ev2Region(r),
			Cluster:      r.Cluster,
			PullNumber:   extractPullNumber(r.URL),
			URL:          r.URL,
		}
	}
	return result
}

func buildFailuresJSON(ranked []rankedFailure, runMap map[int64]JobRun, totalRuns int, s *sippy, release string, windowStart time.Time) []failureJSON {
	// Fetch durations in parallel for top failures
	durCount := min(len(ranked), maxDurationFetches)
	durations := make([][]float64, len(ranked))
	if durCount > 0 {
		type durResult struct {
			idx  int
			vals []float64
		}
		ch := make(chan durResult, durCount)
		for i := range durCount {
			go func(idx int) {
				ch <- durResult{idx, fetchDurations(s, release, ranked[idx].failure.TestName)}
			}(i)
		}
		for range durCount {
			r := <-ch
			durations[r.idx] = r.vals
		}
	}

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
		var normalizedError string
		if len(errorGroups) > 0 {
			dominant := errorGroups[0]
			for _, eg := range errorGroups[1:] {
				if eg.Count > dominant.Count {
					dominant = eg
				}
			}
			normalizedError = normalizeForSimilarity(dominant.Error)
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
			NormalizedError:  normalizedError,
			DailyHits:        buildDailyHits(f, runMap),
			Durations:        durations[i],
			TotalRuns:        totalRuns,
		}
	}
	return result
}

func fetchDurations(s *sippy, release, testName string) []float64 {
	if s == nil || release == "" {
		return nil
	}
	dur, err := s.durations(release, testName)
	if err != nil || len(dur) == 0 {
		return nil
	}
	dates := slices.Sorted(maps.Keys(dur))
	if n := len(dates); n > maxDurationDays {
		dates = dates[n-maxDurationDays:]
	}
	var vals []float64
	for _, d := range dates {
		v := dur[d]
		if v > maxDurationPerDay {
			continue
		}
		vals = append(vals, v)
	}
	return vals
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
		full  string
		count int
	}
	buckets := map[string]*bucket{}
	var order []string

	for _, out := range f.Outputs {
		if out.Output == "" {
			continue
		}
		msg := strings.ReplaceAll(out.Output, "\n", " ")
		msg = strings.Join(strings.Fields(msg), " ")
		msg = stripGoPointers(msg)
		key := stripBase64Blobs(stripTimestamps(stripResourceSuffixes(msg)))

		if b, ok := buckets[key]; ok {
			b.count++
		} else {
			buckets[key] = &bucket{full: msg, count: 1}
			order = append(order, key)
		}
	}

	groups := make([]errorGroupJSON, 0, len(buckets))
	for _, key := range order {
		b := buckets[key]
		groups = append(groups, errorGroupJSON{Error: b.full, Count: b.count})
	}
	return groups
}

// --- Dispatch functions ---

func surveyEnv(s *sippy, env string, days int, jobPat, testPat string, cofailureThreshold float64, blastRadius int) error {
	data, err := fetchSurveyData(s, env, days, jobPat, testPat)
	if err != nil {
		return err
	}
	sj := buildSurveyJSON(env, data, cofailureThreshold, blastRadius, s)
	return json.NewEncoder(os.Stdout).Encode(sj)
}

func surveyAll(s *sippy, days int, jobPat, testPat string, cofailureThreshold float64, blastRadius int) error {
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
			sj := buildSurveyJSON(env, data, cofailureThreshold, blastRadius, s)
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

// buildPRStats computes per-PR pass/fail metrics from presubmit runs.
// Returns nil for non-presubmit environments (where pull_number is always 0).
func buildPRStats(runs []JobRun) *prStatsJSON {

	type prBucket struct {
		passed      int
		failed      int
		failedTests map[string]bool
	}
	prs := map[int]*prBucket{}
	for _, r := range runs {
		pr := extractPullNumber(r.URL)
		if pr == 0 {
			continue
		}
		b, ok := prs[pr]
		if !ok {
			b = &prBucket{failedTests: map[string]bool{}}
			prs[pr] = b
		}
		if realFailureCount(r) == 0 {
			b.passed++
		} else {
			b.failed++
			for _, t := range r.FailedTestNames {
				if !isSyntheticTest(t) {
					b.failedTests[t] = true
				}
			}
		}
	}

	if len(prs) == 0 {
		return nil
	}

	entries := make([]prEntryJSON, 0, len(prs))
	var passRateSum float64
	for pr, b := range prs {
		total := b.passed + b.failed
		rate := 100.0 * float64(b.passed) / float64(total)
		passRateSum += rate
		tests := slices.Sorted(maps.Keys(b.failedTests))
		entries = append(entries, prEntryJSON{
			Number:      pr,
			Runs:        total,
			Passed:      b.passed,
			Failed:      b.failed,
			PassRate:    rate,
			FailedTests: tests,
			Outlier:     total > prOutlierRunThreshold,
			URL:         fmt.Sprintf("https://github.com/Azure/ARO-HCP/pull/%d", pr),
		})
	}

	slices.SortFunc(entries, func(a, b prEntryJSON) int {
		if c := cmp.Compare(b.Failed, a.Failed); c != 0 {
			return c
		}
		return cmp.Compare(b.Runs, a.Runs)
	})

	prWeighted := passRateSum / float64(len(prs))

	var note string
	for _, e := range entries {
		if e.Outlier {
			runWeighted := 100.0 * float64(e.Passed) / float64(e.Runs)
			diff := prWeighted - runWeighted
			if diff > 5 {
				note = fmt.Sprintf("PR #%d has %d runs and skews the run-weighted pass rate. PR-weighted rate (%.1f%%) is more representative.", e.Number, e.Runs, prWeighted)
				break
			}
		}
	}

	return &prStatsJSON{
		DistinctPRs:        len(prs),
		PRWeightedPassRate: prWeighted,
		RunWeightedNote:    note,
		PRs:                entries,
	}
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

// filterNightlyRuns removes ocp-nightly runs from the list.
func filterNightlyRuns(runs []JobRun) []JobRun {
	var filtered []JobRun
	for _, r := range runs {
		if !strings.Contains(r.Job, "ocp-nightly") {
			filtered = append(filtered, r)
		}
	}
	return filtered
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
func rankFailures(failures []RecentFailure, runMap map[int64]JobRun, regularRunIDs map[int64]bool, testPat, jobPat string) []rankedFailure {
	var ranked []rankedFailure
	for _, f := range failures {
		if isSyntheticTest(f.TestName) {
			continue
		}
		if testPat != "" && !strings.Contains(strings.ToLower(f.TestName), strings.ToLower(testPat)) {
			continue
		}
		rf := enrichFailure(f, runMap, regularRunIDs)
		if jobPat == "" && rf.regularHits == 0 && len(f.Outputs) > 0 {
			continue
		}
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
	hash := r.Annotations[ev2HashAnnotation]
	if len(hash) > ev2HashDisplayLen {
		return hash[:ev2HashDisplayLen]
	}
	return hash
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

// extractPipelineStepErrors replaces raw pipeline step log output with just
// the error-level entries, so error_groups shows the actual failure (e.g.,
// "RoleAssignmentLimitExceeded") instead of the full step log starting with
// "Running step."
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
				failures[i].Outputs[j].Output = extracted
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

// --- Error normalization ---

var (
	sourceLocRe = regexp.MustCompile(`^fail \[.*?]:?\s*`)
	uuidRe      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	azureURLRe  = regexp.MustCompile(`https://management\.azure\.com/\S+`)
	numericRe   = regexp.MustCompile(`'\d+[\.\d]*'|\b\d+\.\d+s\b`)
)

func normalizeForSimilarity(s string) string {
	s = sourceLocRe.ReplaceAllLiteralString(s, "")
	s = isoTimestampRe.ReplaceAllString(s, "*")
	s = uuidRe.ReplaceAllString(s, "*")
	s = azureURLRe.ReplaceAllString(s, "*")
	s = numericRe.ReplaceAllString(s, "*")
	s = stripGoPointers(s)
	s = stripResourceSuffixes(s)
	s = strings.ToLower(s)
	return s
}

// --- Utility functions ---

var isoTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
var base64BlobRe = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)

func stripTimestamps(s string) string {
	return isoTimestampRe.ReplaceAllLiteralString(s, "")
}

func stripBase64Blobs(s string) string {
	return base64BlobRe.ReplaceAllLiteralString(s, "")
}

func stripResourceSuffixes(s string) string {
	result := s
	for _, sep := range []string{"resourceGroups/", "resourcegroup=\"", "resource group "} {
		sepLower := strings.ToLower(sep)
		offset := 0
		for offset < len(result) {
			idx := strings.Index(strings.ToLower(result[offset:]), sepLower)
			if idx < 0 {
				break
			}
			start := offset + idx + len(sep)
			end := start
			for end < len(result) && result[end] != '/' && result[end] != '"' && result[end] != ',' && result[end] != ' ' {
				end++
			}
			rgName := result[start:end]
			lastDash := strings.LastIndex(rgName, "-")
			if lastDash <= 0 {
				offset = end
				continue
			}
			cleaned := rgName[:lastDash] + "-*"
			result = result[:start] + cleaned + result[end:]
			offset = start + len(cleaned)
		}
	}
	return result
}

func stripGoPointers(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if n := matchHexAddr(s, i); n > 0 {
			i += n
			continue
		}
		if n := matchTypeAnnotation(s, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func matchHexAddr(s string, i int) int {
	if i+2 >= len(s) || s[i] != '0' || s[i+1] != 'x' {
		return 0
	}
	j := i + 2
	for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || (s[j] >= 'a' && s[j] <= 'f')) {
		j++
	}
	if j-i >= minHexAddrLen {
		return j - i
	}
	return 0
}

func matchTypeAnnotation(s string, i int) int {
	if i+3 >= len(s) || s[i] != '<' || s[i+1] != '*' {
		return 0
	}
	end := strings.Index(s[i:], ">")
	if end <= 0 || end >= maxTypeAnnotationLen {
		return 0
	}
	j := i + end + 1
	for j < len(s) && s[j] == ' ' {
		j++
	}
	if j < len(s) && s[j] == ':' {
		j++
		for j < len(s) && s[j] == ' ' {
			j++
		}
	}
	return j - i
}

// failuresFromRuns synthesizes RecentFailure data from the FailedTestNames
// field on each run. Used as a fallback when the Sippy recent_failures API
// is unavailable.
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

func needsErrorEnrichment(failures []RecentFailure) bool {
	for _, f := range failures {
		hasOutput := false
		for _, out := range f.Outputs {
			if out.Output != "" {
				hasOutput = true
				break
			}
		}
		if !hasOutput {
			return true
		}
	}
	return false
}

type candidateRun struct {
	id    int64
	names []string
}

func collectCandidateRuns(failures []RecentFailure, runs []JobRun) []candidateRun {
	failureNames := map[string]bool{}
	for _, f := range failures {
		if !isSyntheticTest(f.TestName) {
			failureNames[f.TestName] = true
		}
	}
	var candidates []candidateRun
	for _, r := range runs {
		if realFailureCount(r) == 0 {
			continue
		}
		var matched []string
		for _, name := range r.FailedTestNames {
			if failureNames[name] {
				matched = append(matched, name)
			}
		}
		if len(matched) > 0 {
			candidates = append(candidates, candidateRun{id: r.ID, names: matched})
		}
	}
	return candidates
}

// selectEnrichTargets picks runs covering the most distinct failures
// (greedy set-cover bounded by maxTargets).
func selectEnrichTargets(candidates []candidateRun, maxTargets int) []int64 {
	var targets []int64
	covered := map[string]bool{}
	for len(targets) < maxTargets && len(candidates) > 0 {
		bestIdx, bestNew := -1, 0
		for i, c := range candidates {
			newCount := 0
			for _, n := range c.names {
				if !covered[n] {
					newCount++
				}
			}
			if newCount > bestNew {
				bestNew = newCount
				bestIdx = i
			}
		}
		if bestIdx < 0 || bestNew == 0 {
			break
		}
		targets = append(targets, candidates[bestIdx].id)
		for _, n := range candidates[bestIdx].names {
			covered[n] = true
		}
		candidates = append(candidates[:bestIdx], candidates[bestIdx+1:]...)
	}
	return targets
}

func buildErrorMap(results []extensionTestResult) map[string]string {
	errorByTest := map[string]string{}
	for _, t := range results {
		if t.Result != "failed" {
			continue
		}
		errText := t.Error
		if errText == "" && t.Output != "" {
			errText = t.Output
		}
		if errText != "" {
			errorByTest[t.Name] = errText
		}
	}
	return errorByTest
}

func applyErrorEnrichment(failures []RecentFailure, runID int64, errorByTest map[string]string) {
	for i := range failures {
		for j := range failures[i].Outputs {
			if failures[i].Outputs[j].RunID == runID && failures[i].Outputs[j].Output == "" {
				if errText, ok := errorByTest[failures[i].TestName]; ok {
					failures[i].Outputs[j].Output = errText
				}
			}
		}
	}
}

// enrichFailuresWithArtifacts fetches extension_test_result from recent
// failing runs and populates error text into failure outputs. Falls back
// to provision JUnit for pipeline step failures not covered by test results.
func enrichFailuresWithArtifacts(failures []RecentFailure, runs []JobRun) {
	runByID := map[int64]JobRun{}
	for _, r := range runs {
		runByID[r.ID] = r
	}

	candidates := collectCandidateRuns(failures, runs)
	targets := selectEnrichTargets(candidates, maxEnrichTargets)

	fmt.Fprintf(os.Stderr, "enriching: fetching test results from %d runs\n", len(targets))
	store := newGCS()
	for _, runID := range targets {
		r := runByID[runID]
		base := gcsBase(r.URL)
		if base == "" {
			continue
		}
		step, ctr := stepContainer(r.Job)
		results := fetchExtensionResults(store, base, step, ctr)
		if results == nil {
			continue
		}
		applyErrorEnrichment(failures, runID, buildErrorMap(results))
	}

	enrichFromProvisionJUnit(failures, targets, runByID, store)
}

// enrichFromProvisionJUnit fetches provision JUnit XML for pipeline step
// failures that weren't covered by extension_test_result (which only
// contains E2E test results, not pipeline step outputs).
func enrichFromProvisionJUnit(failures []RecentFailure, targets []int64, runByID map[int64]JobRun, store *gcs) {
	unenriched := map[string]bool{}
	for _, f := range failures {
		if !strings.HasPrefix(f.TestName, "Run pipeline step ") {
			continue
		}
		for _, out := range f.Outputs {
			if out.Output == "" {
				unenriched[f.TestName] = true
				break
			}
		}
	}
	if len(unenriched) == 0 {
		return
	}

	fmt.Fprintf(os.Stderr, "enriching: fetching provision JUnit for %d pipeline step failures\n", len(unenriched))
	for _, runID := range targets {
		r := runByID[runID]
		base := gcsBase(r.URL)
		if base == "" {
			continue
		}
		step, _ := stepContainer(r.Job)
		provPath := fmt.Sprintf("artifacts/%s/aro-hcp-provision-environment/artifacts/junit_entrypoint.xml", step)
		data, err := store.artifact(base, provPath)
		if err != nil {
			continue
		}
		suites, err := parseJUnit(data)
		if err != nil {
			continue
		}
		errorByTest := map[string]string{}
		for _, suite := range suites.Suites {
			for _, tc := range suite.Cases {
				if fail := tc.effectiveFailure(); fail != nil {
					if msg := fail.errorMessage(); msg != "" {
						errorByTest[tc.Name] = msg
					}
				}
			}
		}
		applyErrorEnrichment(failures, runID, errorByTest)
	}
}

func fetchExtensionResults(store *gcs, base, step, container string) []extensionTestResult {
	prefix := base + fmt.Sprintf("artifacts/%s/%s/artifacts/", step, container)
	_, files, err := store.listDir(prefix)
	if err != nil {
		return nil
	}
	f := findExtensionResultFile(files)
	if f == "" {
		return nil
	}
	data, err := store.fetch(gcsDownload + f)
	if err != nil {
		return nil
	}
	var tests []extensionTestResult
	if err := json.Unmarshal(data, &tests); err != nil {
		return nil
	}
	return tests
}
