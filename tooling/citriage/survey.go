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
	maxRunsFetch           = 1000
	lowSampleThreshold     = 3
	isolatedFailureCeiling = 3
	moderateFailureCeiling = 15
	neighborRunsLimit        = 100
	maxFailuresWithOutputs   = 20
	maxEnrichmentRuns        = 20
)

func runSurvey(args []string) error {
	fs := flag.NewFlagSet("survey", flag.ExitOnError)
	env := fs.String("env", "all", "environment: int, stg, prod, dev, all")
	days := fs.Int("days", 7, "lookback days")
	job := fs.String("job", "", "override job name filter")
	test := fs.String("test", "", "filter failing tests by name substring")
	fs.Parse(args)

	s := newSippy()
	if *env == "all" {
		return surveyAll(s, *days, *job, *test)
	}
	return surveyEnv(s, *env, *days, *job, *test)
}

type surveyData struct {
	release             string
	runs                []JobRun
	failures            []RecentFailure
	requestedDays       int
	nightlyRunsExcluded int
	runCountCapped      bool
}

// --- JSON output types ---

type surveyJSON struct {
	Env              string                `json:"env"`
	Release          string                `json:"release"`
	Status           statusJSON            `json:"status"`
	DataWindow       *dataWindowJSON       `json:"data_window,omitempty"`
	DailyRates       []dailyRateJSON       `json:"daily_rates"`
	EV2Coverage      ev2CoverageJSON       `json:"ev2_coverage"`
	EV2HashRates     []ev2HashRateJSON     `json:"ev2_hash_rates,omitempty"`
	FailureScaleDist *failureScaleDistJSON `json:"failure_scale_dist,omitempty"`
	RegionRates      []regionRateJSON      `json:"region_rates,omitempty"`
	Runs             []runJSON             `json:"runs"`
	Failures         []failureJSON         `json:"failures"`
}

type dataWindowJSON struct {
	RequestedDays       int    `json:"requested_days"`
	ActualDays          int    `json:"actual_days"`
	OldestRun           string `json:"oldest_run,omitempty"`
	NewestRun           string `json:"newest_run,omitempty"`
	Truncated           bool   `json:"truncated"`
	NightlyRunsExcluded int    `json:"nightly_runs_excluded,omitempty"`
	RunCountCapped      bool   `json:"run_count_capped,omitempty"`
	Empty               bool   `json:"empty,omitempty"`
	EmptyReason         string `json:"empty_reason,omitempty"`
}

type statusJSON struct {
	Streak       int      `json:"streak"`
	CurrentGreen bool     `json:"current_green"`
	StreakRegions []string `json:"streak_regions,omitempty"`
	PassRate     float64  `json:"pass_rate"`
	TotalRuns    int      `json:"total_runs"`
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
	ID           int64  `json:"id"`
	Timestamp    int64  `json:"timestamp"`
	Result       string `json:"overall_result"`
	RealFailures int    `json:"real_failures"`
	EV2Hash      string `json:"ev2_hash,omitempty"`
	Region       string `json:"region,omitempty"`
	Cluster      string `json:"cluster,omitempty"`
	PullNumber   int    `json:"pull_number,omitempty"`
	SHA          string `json:"sha,omitempty"`
	URL          string `json:"url"`
}

type failureJSON struct {
	TestName     string              `json:"test_name"`
	FailureCount int                 `json:"failure_count"`
	FirstFailure string              `json:"first_failure"`
	LastFailure  string              `json:"last_failure"`
	LastPass     string              `json:"last_pass,omitempty"`
	BestRunID    int64               `json:"best_run_id"`
	BestRunURL   string              `json:"best_run_url,omitempty"`
	TotalRuns    int                 `json:"total_runs"`
	Outputs      []failureOutputJSON `json:"outputs"`
}

type failureOutputJSON struct {
	RunID           int64  `json:"run_id"`
	Error           string `json:"error,omitempty"`
	ExtractedErrors string `json:"extracted_errors,omitempty"`
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
	None     int `json:"none"`
	Isolated int `json:"isolated"`
	Moderate int `json:"moderate"`
	Cascade  int `json:"cascade"`
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
	failures = filterFailuresToRuns(failures, runs)
	enrichMissingErrors(failures, runs)
	enrichPipelineStepErrors(s, failures, runs)
	extractPipelineStepErrors(failures)
	enrichLastPass(failures, runs)

	if testPat != "" {
		var filtered []RecentFailure
		for _, f := range failures {
			if strings.Contains(strings.ToLower(f.TestName), strings.ToLower(testPat)) {
				filtered = append(filtered, f)
			}
		}
		failures = filtered
	}

	return &surveyData{
		release:             release,
		runs:                runs,
		failures:            failures,
		requestedDays:       days,
		nightlyRunsExcluded: nightlyExcluded,
		runCountCapped:      runCountCapped,
	}, nil
}

// --- JSON building ---

func buildSurveyJSON(env string, data *surveyData) surveyJSON {
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
			Streak:       streak,
			CurrentGreen: green,
			StreakRegions: slices.Sorted(maps.Keys(regions)),
			PassRate:     100.0 * float64(passed) / float64(len(data.runs)),
			TotalRuns:    len(data.runs),
		}
	}

	result.DataWindow = buildDataWindow(data.runs, data.requestedDays, data.nightlyRunsExcluded, data.runCountCapped)
	result.DailyRates = buildDailyRates(data.runs)
	result.EV2Coverage = buildEV2Coverage(data.runs)
	result.EV2HashRates = buildEV2HashRates(data.runs)
	result.FailureScaleDist = buildFailureScaleDist(data.runs)
	result.RegionRates = buildRegionRates(data.runs)
	result.Runs = buildRunsJSON(data.runs)
	result.Failures = buildFailuresJSON(data.failures, data.runs)

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
	var result []runJSON
	for _, r := range runs {
		fc := realFailureCount(r)
		if fc == 0 {
			continue
		}
		result = append(result, runJSON{
			ID:           r.ID,
			Timestamp:    r.Timestamp,
			Result:       r.OverallResult,
			RealFailures: fc,
			EV2Hash:      ev2Hash(r),
			Region:       ev2Region(r),
			Cluster:      r.Cluster,
			PullNumber:   extractPullNumber(r.URL),
			SHA:          r.PullRequestSHA,
			URL:          r.URL,
		})
	}
	return result
}

func buildFailuresJSON(failures []RecentFailure, runs []JobRun) []failureJSON {
	urlByID := map[int64]string{}
	for _, r := range runs {
		urlByID[r.ID] = r.URL
	}

	var result []failureJSON
	for _, f := range failures {
		if isSyntheticTest(f.TestName) {
			continue
		}
		var outputs []failureOutputJSON
		for _, out := range f.Outputs {
			outputs = append(outputs, failureOutputJSON{
				RunID:           out.RunID,
				Error:           out.Output,
				ExtractedErrors: out.ExtractedErrors,
			})
		}
		bestRunID := int64(0)
		bestRunURL := ""
		if len(f.Outputs) > 0 {
			bestRunID = f.Outputs[0].RunID
			bestRunURL = urlByID[bestRunID]
		}
		result = append(result, failureJSON{
			TestName:     f.TestName,
			FailureCount: f.FailureCount,
			FirstFailure: f.FirstFailure,
			LastFailure:  f.LastFailure,
			LastPass:     f.LastPass,
			BestRunID:    bestRunID,
			BestRunURL:   bestRunURL,
			TotalRuns:    len(runs),
			Outputs:      outputs,
		})
	}
	slices.SortFunc(result, func(a, b failureJSON) int {
		return cmp.Compare(b.FailureCount, a.FailureCount)
	})
	for i := maxFailuresWithOutputs; i < len(result); i++ {
		result[i].Outputs = nil
	}
	return result
}

// --- Dispatch ---

func surveyEnv(s *sippy, env string, days int, jobPat, testPat string) error {
	data, err := fetchSurveyData(s, env, days, jobPat, testPat)
	if err != nil {
		return err
	}
	sj := buildSurveyJSON(env, data)
	return json.NewEncoder(os.Stdout).Encode(sj)
}

func surveyAll(s *sippy, days int, jobPat, testPat string) error {
	envs := []string{"int", "stg", "prod"}
	type envResult struct {
		env      string
		survey   surveyJSON
		failures []RecentFailure
		err      error
	}
	ch := make(chan envResult, len(envs))
	for _, e := range envs {
		go func(env string) {
			data, err := fetchSurveyData(s, env, days, jobPat, testPat)
			if err != nil {
				ch <- envResult{env: env, err: err}
				return
			}
			sj := buildSurveyJSON(env, data)
			ch <- envResult{env: env, survey: sj, failures: data.failures}
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
		for _, f := range r.failures {
			if isSyntheticTest(f.TestName) {
				continue
			}
			bestRunID := int64(0)
			if len(f.Outputs) > 0 {
				bestRunID = f.Outputs[0].RunID
			}
			testEnvEntries[f.TestName] = append(testEnvEntries[f.TestName], crossEnvEntryJSON{
				Env:   e,
				Hits:  f.FailureCount,
				RunID: bestRunID,
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

func collectRegions(runs []JobRun) map[string]bool {
	regions := map[string]bool{}
	for _, r := range runs {
		if reg := ev2Region(r); reg != "" {
			regions[reg] = true
		}
	}
	return regions
}

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

var logEntryStartRe = regexp.MustCompile(`time=\d{4}-\d{2}-\d{2}T`)

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

// --- Error enrichment ---

func enrichMissingErrors(failures []RecentFailure, runs []JobRun) {
	needRunIDs := map[int64]bool{}
	for _, f := range failures {
		if strings.HasPrefix(f.TestName, "Run pipeline step ") {
			continue
		}
		for _, out := range f.Outputs {
			if out.Output == "" {
				needRunIDs[out.RunID] = true
			}
		}
	}
	if len(needRunIDs) == 0 {
		return
	}

	runByID := map[int64]JobRun{}
	for _, r := range runs {
		runByID[r.ID] = r
	}
	var targets []JobRun
	for id := range needRunIDs {
		if r, ok := runByID[id]; ok {
			targets = append(targets, r)
		}
	}
	if len(targets) > maxEnrichmentRuns {
		targets = targets[:maxEnrichmentRuns]
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

// enrichPipelineStepErrors fills in error text for "Run pipeline step ..." failures
// by fetching per-run summaries from the Sippy API. The RunSummary.TestFailures
// map uses exact Sippy test names as keys, avoiding the name-matching problem
// with JUnit XML (which uses ci-operator step names, not pipeline step paths).
func enrichPipelineStepErrors(s *sippy, failures []RecentFailure, _ []JobRun) {
	needRunIDs := map[int64]bool{}
	stepFailureIdx := map[string][]int{}
	for i, f := range failures {
		if !strings.HasPrefix(f.TestName, "Run pipeline step ") {
			continue
		}
		for _, out := range f.Outputs {
			if out.Output == "" {
				stepFailureIdx[f.TestName] = append(stepFailureIdx[f.TestName], i)
				needRunIDs[out.RunID] = true
			}
		}
	}
	if len(needRunIDs) == 0 {
		return
	}

	runIDs := make([]int64, 0, len(needRunIDs))
	for id := range needRunIDs {
		runIDs = append(runIDs, id)
	}
	if len(runIDs) > maxEnrichmentRuns {
		runIDs = runIDs[:maxEnrichmentRuns]
	}

	type fetchResult struct {
		runID  int64
		errors map[string]string
	}
	ch := make(chan fetchResult, len(runIDs))
	for _, id := range runIDs {
		go func(rid int64) {
			summary, err := s.runSummary(fmt.Sprintf("%d", rid))
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: run summary fetch failed for %d: %v\n", rid, err)
				ch <- fetchResult{rid, nil}
				return
			}
			ch <- fetchResult{rid, summary.TestFailures}
		}(id)
	}

	errorsByRunAndTest := map[int64]map[string]string{}
	for range runIDs {
		r := <-ch
		if r.errors != nil {
			errorsByRunAndTest[r.runID] = r.errors
		}
	}

	for testName, indices := range stepFailureIdx {
		for _, idx := range indices {
			f := &failures[idx]
			for j := range f.Outputs {
				if f.Outputs[j].Output != "" {
					continue
				}
				if m, ok := errorsByRunAndTest[f.Outputs[j].RunID]; ok {
					if errText, ok := m[testName]; ok && errText != "" {
						f.Outputs[j].Output = errText
					}
				}
			}
		}
	}
}

func enrichLastPass(failures []RecentFailure, runs []JobRun) {
	for i := range failures {
		if failures[i].LastPass != "" {
			continue
		}
		name := failures[i].TestName
		for _, r := range runs {
			if len(r.FailedTestNames) == 0 {
				continue
			}
			if !slices.Contains(r.FailedTestNames, name) {
				failures[i].LastPass = time.UnixMilli(r.Timestamp).UTC().Format(time.RFC3339)
				break
			}
		}
	}
}

// filterFailuresToRuns keeps only failures whose outputs reference runs in our
// job-filtered runs list. Sippy's recentFailures API for "Presubmits" returns
// failures across ALL presubmit jobs; this scopes them to our specific jobs.
func filterFailuresToRuns(failures []RecentFailure, runs []JobRun) []RecentFailure {
	runIDs := map[int64]bool{}
	for _, r := range runs {
		runIDs[r.ID] = true
	}

	// Also build a set of test names that appear in our runs' FailedTestNames
	testNames := map[string]bool{}
	for _, r := range runs {
		for _, name := range r.FailedTestNames {
			testNames[name] = true
		}
	}

	var filtered []RecentFailure
	for _, f := range failures {
		// Keep if any output references one of our runs
		hasMatchingRun := false
		for _, out := range f.Outputs {
			if runIDs[out.RunID] {
				hasMatchingRun = true
				break
			}
		}
		// Or if the test name appears in our runs' failure lists
		if !hasMatchingRun && testNames[f.TestName] {
			hasMatchingRun = true
		}
		if hasMatchingRun {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

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
