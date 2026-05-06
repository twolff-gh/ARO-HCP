package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// digContext holds state for fetching artifacts from a single CI run.
type digContext struct {
	out       io.Writer
	store     *gcs
	base      string
	step      string
	container string
	summary   *RunSummary
}

// digEnvelope wraps all dig subcommand output with run metadata.
type digEnvelope struct {
	RunID      string `json:"run_id"`
	Job        string `json:"job"`
	Result     string `json:"result"`
	ProwURL    string `json:"prow_url"`
	Subcommand string `json:"subcommand"`
	Data       any    `json:"data"`
}

func (d *digContext) emitJSON(subcommand string, data any) error {
	return json.NewEncoder(d.out).Encode(digEnvelope{
		RunID:      fmt.Sprintf("%d", d.summary.ID),
		Job:        d.summary.Name,
		Result:     d.summary.OverallResult,
		ProwURL:    d.summary.URL,
		Subcommand: subcommand,
		Data:       data,
	})
}

// newDigContext initializes a dig session by resolving the run summary and GCS paths.
func newDigContext(runID string) (*digContext, error) {
	s := newSippy()
	summary, err := s.runSummary(runID)
	if err != nil {
		return nil, fmt.Errorf("run summary: %w", err)
	}
	base := gcsBase(summary.URL)
	if base == "" {
		return nil, fmt.Errorf("cannot derive GCS path from %s", summary.URL)
	}
	store := newGCS()
	step, candidates := stepContainer(summary.Name)
	ctr := resolveContainer(store, base, step, candidates)
	return &digContext{
		out: os.Stdout, store: store,
		base: base, step: step, container: ctr, summary: summary,
	}, nil
}

// runDig dispatches to the appropriate dig subcommand for artifact fetching.
func runDig(args []string) error {
	fs := flag.NewFlagSet("dig", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return fmt.Errorf("usage: dig <run-id> <what> [args...]")
	}
	runID, what := pos[0], pos[1]
	rest := pos[2:]

	dig, err := newDigContext(runID)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "run: %s | job: %s | result: %s\nprow: %s\n",
		runID, dig.summary.Name, dig.summary.OverallResult, dig.summary.URL)

	switch what {
	case "tests":
		return dig.Tests()
	case "azure":
		if len(rest) == 0 {
			return fmt.Errorf("usage: dig <run-id> azure <test-name>")
		}
		return dig.Azure(strings.Join(rest, " "))
	case "metrics":
		return dig.Metrics()
	case "provision":
		return dig.Provision()
	case "alerts":
		return dig.Alerts()
	case "events":
		return dig.Events()
	case "pool":
		return dig.Pool()
	case "podinfo":
		return dig.Podinfo()
	case "steptime":
		var baseline string
		if len(rest) > 0 {
			baseline = rest[0]
		}
		return dig.StepTime(baseline)
	case "links":
		return dig.Links()
	default:
		return fmt.Errorf("unknown dig subcommand: %s", what)
	}
}

func isPresubmitJob(name string) bool {
	return strings.Contains(name, "pull-ci-")
}

func stepContainer(job string) (string, []string) {
	switch {
	case strings.Contains(job, "integration"):
		return "integration-e2e-parallel", []string{"aro-hcp-test-persistent"}
	case strings.Contains(job, "stage"):
		return "stage-e2e-parallel", []string{"aro-hcp-test-persistent"}
	case strings.Contains(job, "prod"):
		return "prod-e2e-parallel", []string{"aro-hcp-test-persistent"}
	default:
		return "e2e-parallel", []string{"aro-hcp-test-local", "aro-hcp-test-local-run"}
	}
}

// resolveContainer probes GCS to find which container name has artifacts.
// Returns the first candidate whose artifact directory exists, or the first
// candidate as a default.
func resolveContainer(store *gcs, base, step string, candidates []string) string {
	if len(candidates) == 1 {
		return candidates[0]
	}
	for _, c := range candidates {
		prefix := base + fmt.Sprintf("artifacts/%s/%s/artifacts/", step, c)
		if dirs, _, err := store.listDir(prefix); err == nil && len(dirs) > 0 {
			return c
		}
	}
	return candidates[0]
}

func (d *digContext) testArtifactPrefix() string {
	return d.base + fmt.Sprintf("artifacts/%s/%s/artifacts/", d.step, d.container)
}

func (d *digContext) testArtifactDir(testName string) string {
	return fmt.Sprintf("artifacts/%s/%s/artifacts/%s/", d.step, d.container, sanitizeTest(testName))
}

// extensionTestResult is a single test result from the extension_test_result JSON artifact.
type extensionTestResult struct {
	Name      string `json:"name"`
	Result    string `json:"result"`
	Duration  int64  `json:"duration"`
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
	Error     string `json:"error"`
	Output    string `json:"output"`
}

type testResultJSON struct {
	Name        string  `json:"name"`
	Result      string  `json:"result"`
	DurationSec float64 `json:"duration_seconds"`
	StartTime   string  `json:"start_time"`
	EndTime     string  `json:"end_time"`
	Error       string  `json:"error,omitempty"`
	OutputLines int     `json:"output_lines"`
	ArtifactURL string  `json:"artifact_url"`
	Corrupted   bool    `json:"corrupted"`
}

type testsJSON struct {
	ArtifactURL string           `json:"artifact_url"`
	Source      string           `json:"source"`
	Tests       []testResultJSON `json:"tests"`
	Failed      int              `json:"failed"`
	Passed      int              `json:"passed"`
	Skipped     int              `json:"skipped"`
}

func (d *digContext) Tests() error {
	_, files, err := d.store.listDir(d.testArtifactPrefix())
	if err != nil {
		files = nil
	}
	if f := findExtensionResultFile(files); f != "" {
		return d.testsFromExtension(gcsDownload + f)
	}
	if data, err := d.store.artifact(d.base, "artifacts/junit_operator.xml"); err == nil {
		return d.testsFromJUnit(data, "junit_operator.xml")
	}
	return d.emitJSON("tests", noResultsJSON{Message: "No test results found"})
}

func (d *digContext) testsFromExtension(extURL string) error {
	data, err := d.store.fetch(extURL)
	if err != nil {
		return err
	}

	var tests []extensionTestResult
	if err := json.Unmarshal(data, &tests); err != nil {
		return fmt.Errorf("parsing extension_test_result: %w", err)
	}

	artURL := strings.Replace(extURL, gcsDownload, gcsWeb, 1)
	var failed, passed, skipped int
	var results []testResultJSON
	for _, t := range tests {
		switch t.Result {
		case "failed":
			failed++
		case "passed":
			passed++
		default:
			skipped++
			continue // exclude skipped from output
		}
		outputLines := strings.Count(t.Output, "\n")
		r := testResultJSON{
			Name:        t.Name,
			Result:      t.Result,
			DurationSec: float64(t.Duration) / 1000.0,
			StartTime:   t.StartTime,
			EndTime:     t.EndTime,
			Error:       t.Error,
			OutputLines: outputLines,
			ArtifactURL: gcsWeb + d.base + d.testArtifactDir(t.Name),
			Corrupted:   t.StartTime == t.EndTime,
		}
		results = append(results, r)
	}
	return d.emitJSON("tests", testsJSON{
		ArtifactURL: artURL,
		Source:      "extension_test_result",
		Tests:       results,
		Failed:      failed,
		Passed:      passed,
		Skipped:     skipped,
	})
}

func (d *digContext) loadTestResults() ([]extensionTestResult, error) {
	_, files, err := d.store.listDir(d.testArtifactPrefix())
	if err != nil {
		return nil, err
	}
	extFile := findExtensionResultFile(files)
	if extFile == "" {
		return nil, fmt.Errorf("no extension_test_result found")
	}
	data, err := d.store.fetch(gcsDownload + extFile)
	if err != nil {
		return nil, err
	}
	var tests []extensionTestResult
	if err := json.Unmarshal(data, &tests); err != nil {
		return nil, err
	}
	return tests, nil
}

// --- JUnit parsing (shared by test fallback and provision) ---

type junitTestSuites struct {
	XMLName xml.Name         `xml:"testsuites"`
	Suites  []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name     string          `xml:"name,attr"`
	Tests    int             `xml:"tests,attr"`
	Failures int             `xml:"failures,attr"`
	Cases    []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	Name    string        `xml:"name,attr"`
	Time    float64       `xml:"time,attr"`
	Failure *junitFailure `xml:"failure"`
	Error   *junitFailure `xml:"error"`
	Skipped *struct{}     `xml:"skipped"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

func (tc junitTestCase) effectiveFailure() *junitFailure {
	if tc.Failure != nil {
		return tc.Failure
	}
	return tc.Error
}

func (f *junitFailure) errorMessage() string {
	if f == nil {
		return ""
	}
	if f.Message != "" {
		return f.Message
	}
	return f.Body
}

func parseJUnit(data []byte) (junitTestSuites, error) {
	var suites junitTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil || len(suites.Suites) == 0 {
		var single junitTestSuite
		if err2 := xml.Unmarshal(data, &single); err2 != nil {
			if err != nil {
				return suites, fmt.Errorf("failed to parse JUnit XML: %w", err)
			}
			return suites, fmt.Errorf("failed to parse JUnit XML: %w", err2)
		}
		suites.Suites = []junitTestSuite{single}
	}
	return suites, nil
}

func (d *digContext) testsFromJUnit(data []byte, source string) error {
	suites, err := parseJUnit(data)
	if err != nil {
		return err
	}

	var results []testResultJSON
	var failed, passed, skipped int
	for _, suite := range suites.Suites {
		for _, tc := range suite.Cases {
			if tc.Skipped != nil {
				skipped++
				continue
			}
			r := testResultJSON{Name: tc.Name, DurationSec: tc.Time}
			if fail := tc.effectiveFailure(); fail != nil {
				failed++
				r.Result = "failed"
				r.Error = fail.errorMessage()
			} else {
				passed++
				r.Result = "passed"
			}
			results = append(results, r)
		}
	}
	return d.emitJSON("tests", testsJSON{
		Source:  source,
		Tests:   results,
		Failed:  failed,
		Passed:  passed,
		Skipped: skipped,
	})
}

// --- Step graph fallback ---

type noResultsJSON struct {
	Message string `json:"message"`
}

type eventsJSON struct {
	ArtifactURL string          `json:"artifact_url"`
	Events      json.RawMessage `json:"events"`
}

type podinfoJSON struct {
	ArtifactURL string          `json:"artifact_url"`
	Pod         json.RawMessage `json:"pod"`
	OOMDetected bool            `json:"oom_detected"`
}

type messageJSON struct {
	Message string `json:"message"`
}

// --- Azure ---

type azureLogEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Event string `json:"event"`
	Msg   string `json:"msg"`
}

type azureJSON struct {
	TestName    string          `json:"test_name"`
	ArtifactURL string         `json:"artifact_url"`
	TotalLines  int            `json:"total_lines"`
	ErrorCount  int            `json:"error_count"`
	FirstTime   string         `json:"first_time"`
	LastTime    string         `json:"last_time"`
	Entries     []azureLogEntry `json:"entries"`
}

func (d *digContext) Azure(testName string) error {
	path := fmt.Sprintf("artifacts/%s/%s/artifacts/%s/azure.log",
		d.step, d.container, sanitizeTest(testName))
	data, err := d.store.artifact(d.base, path)
	if err != nil {
		return err
	}

	rawLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entries []azureLogEntry
	var firstTime, lastTime string
	errorCount := 0
	for _, line := range rawLines {
		var entry azureLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
		if firstTime == "" {
			firstTime = entry.Time
		}
		lastTime = entry.Time
		if entry.Level == "ERROR" || entry.Event == "ResponseError" {
			errorCount++
		}
	}

	return d.emitJSON("azure", azureJSON{
		TestName:    testName,
		ArtifactURL: gcsWeb + d.base + path,
		TotalLines:  len(rawLines),
		ErrorCount:  errorCount,
		FirstTime:   firstTime,
		LastTime:    lastTime,
		Entries:     entries,
	})
}

// --- Metrics (pass-through) ---

func (d *digContext) Metrics() error {
	data, err := d.store.artifact(d.base, "artifacts/ci-operator-metrics.json")
	if err != nil {
		return err
	}
	if json.Valid(data) {
		return d.emitJSON("metrics", json.RawMessage(data))
	}
	return d.emitJSON("metrics", map[string]string{"raw": string(data)})
}

// --- Events (pass-through with item extraction) ---

func (d *digContext) Events() error {
	data, err := d.store.artifact(d.base, "artifacts/build-resources/events.json")
	if err != nil {
		return err
	}
	var list struct{ Items json.RawMessage `json:"items"` }
	if err := json.Unmarshal(data, &list); err != nil || list.Items == nil {
		return d.emitJSON("events", json.RawMessage(data))
	}
	return d.emitJSON("events", eventsJSON{
		ArtifactURL: gcsWeb + d.base + "artifacts/build-resources/events.json",
		Events:      list.Items,
	})
}

// --- Podinfo (pass-through with OOM detection) ---

func (d *digContext) Podinfo() error {
	data, err := d.store.artifact(d.base, "podinfo.json")
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return d.emitJSON("podinfo", map[string]string{"raw": string(data)})
	}
	summary := extractPodinfoSummary(data)
	oom := summary != nil && summary.OOMDetected
	return d.emitJSON("podinfo", podinfoJSON{
		ArtifactURL: gcsWeb + d.base + "podinfo.json",
		Pod:         json.RawMessage(data),
		OOMDetected: oom,
	})
}

// --- Pool ---

type poolJSON struct {
	ArtifactURL     string   `json:"artifact_url"`
	TotalContainers int      `json:"total_containers"`
	Free            int      `json:"free"`
	Assigned        int      `json:"assigned"`
	Busy            int      `json:"busy"`
	Contention      []string `json:"contention,omitempty"`
}

func (d *digContext) Pool() error {
	poolPath := fmt.Sprintf("artifacts/%s/%s/artifacts/identities-pool-state.yaml",
		d.step, d.container)
	data, err := d.store.artifact(d.base, poolPath)
	if err != nil {
		return err
	}
	ps := extractPoolSummary(data)
	return d.emitJSON("pool", poolJSON{
		ArtifactURL:     gcsWeb + d.base + poolPath,
		TotalContainers: ps.Total,
		Free:            ps.Free,
		Assigned:        ps.Assigned,
		Busy:            ps.Busy,
		Contention:      ps.Contention,
	})
}

// --- Provision ---

type provisionCaseJSON struct {
	Name    string  `json:"name"`
	Time    float64 `json:"time"`
	Failed  bool    `json:"failed"`
	Message string  `json:"message,omitempty"`
}

type provisionSuiteJSON struct {
	Name     string              `json:"name"`
	Tests    int                 `json:"tests"`
	Failures int                 `json:"failures"`
	Cases    []provisionCaseJSON `json:"cases"`
}

type provisionJSON struct {
	ArtifactURL string               `json:"artifact_url"`
	Suites      []provisionSuiteJSON `json:"suites"`
}

func (d *digContext) Provision() error {
	if !isPresubmitJob(d.summary.Name) {
		return d.emitJSON("provision", messageJSON{
			Message: "Provision artifacts not available (periodic runs don't provision infrastructure).",
		})
	}

	provPath := fmt.Sprintf("artifacts/%s/aro-hcp-provision-environment/artifacts/junit_entrypoint.xml", d.step)
	data, err := d.store.artifact(d.base, provPath)
	if err != nil {
		return err
	}

	suites, err := parseJUnit(data)
	if err != nil {
		return err
	}

	var out provisionJSON
	out.ArtifactURL = gcsWeb + d.base + provPath
	for _, suite := range suites.Suites {
		s := provisionSuiteJSON{Name: suite.Name, Tests: suite.Tests, Failures: suite.Failures}
		for _, tc := range suite.Cases {
			c := provisionCaseJSON{Name: tc.Name, Time: tc.Time}
			if fail := tc.effectiveFailure(); fail != nil {
				c.Failed = true
				c.Message = fail.errorMessage()
			}
			s.Cases = append(s.Cases, c)
		}
		out.Suites = append(out.Suites, s)
	}
	return d.emitJSON("provision", out)
}

// --- Alerts (pass-through) ---

func (d *digContext) Alerts() error {
	if !isPresubmitJob(d.summary.Name) {
		return d.emitJSON("alerts", messageJSON{
			Message: "Alerts artifacts not available (periodic runs don't have Azure Monitor alert snapshots).",
		})
	}

	data, err := d.store.artifact(d.base,
		fmt.Sprintf("artifacts/%s/aro-hcp-gather-observability/artifacts/alerts.json", d.step))
	if err != nil {
		return err
	}
	if json.Valid(data) {
		return d.emitJSON("alerts", json.RawMessage(data))
	}
	return d.emitJSON("alerts", map[string]string{"raw": string(data)})
}

// --- Raw ---

// --- StepTime: step durations with optional baseline ---

type stepTimeJSON struct {
	Steps    []StepTiming `json:"steps"`
	TotalSec float64      `json:"total_seconds"`
	Baseline string       `json:"baseline_run,omitempty"`
}

func (d *digContext) StepTime(baselineRunID string) error {
	data, err := d.store.artifact(d.base, "artifacts/ci-operator-step-graph.json")
	if err != nil {
		return err
	}
	steps := extractStepTimings(data)
	if steps == nil {
		return fmt.Errorf("parsing step graph")
	}

	if junitData, err := d.store.artifact(d.base, "artifacts/junit_operator.xml"); err == nil {
		enrichStepsWithJUnit(steps, junitData)
	}

	if baselineRunID != "" {
		s := newSippy()
		if baseSummary, err := s.runSummary(baselineRunID); err == nil {
			baseBase := gcsBase(baseSummary.URL)
			if baseBase != "" {
				if baseData, err := d.store.artifact(baseBase, "artifacts/ci-operator-step-graph.json"); err == nil {
					applyStepBaseline(steps, baseData)
				}
			}
		}
	}

	total := 0.0
	for _, s := range steps {
		total += s.DurationSec
	}

	return d.emitJSON("steptime", stepTimeJSON{
		Steps:    steps,
		TotalSec: total,
		Baseline: baselineRunID,
	})
}

// --- Links: test-to-resource-group mapping ---

func (d *digContext) Links() error {
	path := fmt.Sprintf("artifacts/%s/aro-hcp-gather-custom-link-tools/artifacts/custom-link-tools-commands.html", d.step)
	data, err := d.store.artifact(d.base, path)
	if err != nil {
		return d.emitJSON("links", messageJSON{Message: "custom-link-tools not found"})
	}
	links := extractTestLinks(string(data))
	return d.emitJSON("links", links)
}

