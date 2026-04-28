package main

import (
	"encoding/json"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

const (
	buildLogTailLines   = 15
)

// BuildLogExtract holds structural signals from a CI build log.
type BuildLogExtract struct {
	FileSizeBytes int      `json:"file_size_bytes"`
	ErrorLines    []string `json:"error_lines,omitempty"`
	StepLines     []string `json:"step_lines,omitempty"`
	TailLines     []string `json:"tail_lines,omitempty"`
	TestFailCount int      `json:"test_fail_count,omitempty"`
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllLiteralString(s, "")
}

var (
	buildLogErrorRe     = regexp.MustCompile(`(?i)^(ERRO|ERROR|FATAL)\b`)
	buildLogStepRe      = regexp.MustCompile(`(?i)Step\s+\S+\s+(failed|succeeded)\s+after\s+\S+`)
	buildLogFailCountRe = regexp.MustCompile(`(\d+)\s+tests?\s+failed`)
)

// extractBuildLog parses a CI build log for error/step lines, tail, and test failure count.
func extractBuildLog(raw []byte) *BuildLogExtract {
	content := stripANSI(string(raw))
	lines := strings.Split(content, "\n")
	result := &BuildLogExtract{
		FileSizeBytes: len(raw),
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if buildLogErrorRe.MatchString(trimmed) {
			result.ErrorLines = append(result.ErrorLines, trimmed)
		}
		if buildLogStepRe.MatchString(trimmed) {
			result.StepLines = append(result.StepLines, trimmed)
		}
	}

	tailN := buildLogTailLines
	if len(lines) > tailN {
		tail := lines[len(lines)-tailN:]
		for _, l := range tail {
			t := strings.TrimSpace(l)
			if t != "" {
				result.TailLines = append(result.TailLines, t)
			}
		}
	} else {
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t != "" {
				result.TailLines = append(result.TailLines, t)
			}
		}
	}

	if m := buildLogFailCountRe.FindStringSubmatch(content); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			result.TestFailCount = n
		}
	}

	return result
}

// --- Metrics event extraction ---

// MetricsExtract holds parsed events and resource-acquisition latencies from ci-operator-metrics.json.
type MetricsExtract struct {
	Events         []MetricsEvent `json:"events"`
	MaxLeaseAcqSec float64        `json:"max_lease_acquisition_seconds"`
	MaxPodSchedSec float64        `json:"max_pod_scheduling_seconds"`
}

// MetricsEvent is a single step-level event from ci-operator metrics.
type MetricsEvent struct {
	StepName    string  `json:"step_name"`
	Level       string  `json:"level"`
	Success     bool    `json:"success"`
	DurationSec float64 `json:"duration_seconds"`
	Cause       string  `json:"cause,omitempty"`
	HumanMessage string `json:"human_message,omitempty"`
}

type rawMetrics struct {
	Events []struct {
		Level   string `json:"level"`
		Locator struct {
			Name string `json:"name"`
		} `json:"locator"`
		Message struct {
			Reason      string `json:"reason"`
			Cause       string `json:"cause"`
			HumanMessage string `json:"humanMessage"`
			Annotations struct {
				DurationSec float64 `json:"duration_seconds"`
				Success     bool    `json:"success"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"events"`
	Leases []struct {
		AcquisitionDurationSeconds float64 `json:"acquisition_duration_seconds"`
	} `json:"leases"`
	Pods []struct {
		SchedulingLatency int64 `json:"scheduling_latency"`
	} `json:"pods"`
}

// extractMetricsEvents parses ci-operator-metrics.json for step events and resource latencies.
func extractMetricsEvents(data []byte) *MetricsExtract {
	var raw rawMetrics
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}

	result := &MetricsExtract{}

	for _, e := range raw.Events {
		me := MetricsEvent{
			StepName:     e.Locator.Name,
			Level:        e.Level,
			Success:      e.Message.Annotations.Success,
			DurationSec:  e.Message.Annotations.DurationSec,
			HumanMessage: e.Message.HumanMessage,
		}
		if e.Message.Cause != "" {
			me.Cause = e.Message.Cause
		}
		result.Events = append(result.Events, me)
	}

	for _, l := range raw.Leases {
		if l.AcquisitionDurationSeconds > result.MaxLeaseAcqSec {
			result.MaxLeaseAcqSec = l.AcquisitionDurationSeconds
		}
	}

	for _, p := range raw.Pods {
		sec := float64(p.SchedulingLatency) / 1e9
		if sec > result.MaxPodSchedSec {
			result.MaxPodSchedSec = sec
		}
	}

	return result
}

// --- Step-graph extraction ---

// StepTiming holds duration and status for one CI pipeline step.
type StepTiming struct {
	Name         string  `json:"name"`
	DurationSec  float64 `json:"duration_seconds"`
	Failed       bool    `json:"failed"`
	BaselineSec  float64 `json:"baseline_seconds,omitempty"`
	Ratio        float64 `json:"ratio,omitempty"`
	ExitCode     int     `json:"exit_code,omitempty"`
	ErrorSnippet string  `json:"error_snippet,omitempty"`
}

type rawStep struct {
	Name     string `json:"name"`
	Duration int64  `json:"duration"`
	Failed   bool   `json:"failed"`
}

// extractStepTimings parses ci-operator-step-graph.json into step durations.
func extractStepTimings(data []byte) []StepTiming {
	var raw []json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}

	var steps []StepTiming
	for _, entry := range raw {
		var s rawStep
		if json.Unmarshal(entry, &s) != nil {
			continue
		}
		dur := float64(s.Duration) / 1e9
		if dur <= 0 && !s.Failed {
			continue
		}
		steps = append(steps, StepTiming{
			Name:        s.Name,
			DurationSec: dur,
			Failed:      s.Failed,
		})
	}
	return steps
}

// enrichStepsWithJUnit adds error snippets from junit_operator.xml to matching step entries.
func enrichStepsWithJUnit(steps []StepTiming, junitData []byte) {
	var suites junitTestSuites
	if xml.Unmarshal(junitData, &suites) != nil {
		var suite junitTestSuite
		if xml.Unmarshal(junitData, &suite) != nil {
			return
		}
		suites.Suites = []junitTestSuite{suite}
	}

	failMap := map[string]junitTestCase{}
	for _, suite := range suites.Suites {
		for _, tc := range suite.Cases {
			if tc.effectiveFailure() != nil {
				failMap[tc.Name] = tc
			}
		}
	}

	for i := range steps {
		for name, tc := range failMap {
			if strings.Contains(name, steps[i].Name) || strings.Contains(steps[i].Name, name) {
				if f := tc.effectiveFailure(); f != nil {
					steps[i].ErrorSnippet = stripANSI(f.errorMessage())
				}
				break
			}
		}
	}
}

// applyStepBaseline computes duration ratios by comparing against a baseline run's step graph.
func applyStepBaseline(steps []StepTiming, baselineData []byte) {
	baseSteps := extractStepTimings(baselineData)
	if baseSteps == nil {
		return
	}
	baseMap := map[string]float64{}
	for _, s := range baseSteps {
		baseMap[s.Name] = s.DurationSec
	}
	for i := range steps {
		if base, ok := baseMap[steps[i].Name]; ok && base > 0 {
			steps[i].BaselineSec = base
			steps[i].Ratio = steps[i].DurationSec / base
		}
	}
}

// --- Podinfo summary extraction ---

// PodinfoSummary extracts container exit status and OOM detection from podinfo.json.
type PodinfoSummary struct {
	ExitCode    int    `json:"exit_code"`
	Reason      string `json:"reason"`
	OOMDetected bool   `json:"oom_detected"`
}

func extractPodinfoSummary(data []byte) *PodinfoSummary {
	var pod struct {
		Pod struct {
			Status struct {
				ContainerStatuses []struct {
					Name  string                        `json:"name"`
					State map[string]map[string]any `json:"state"`
					LastState map[string]map[string]any `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"pod"`
	}
	if json.Unmarshal(data, &pod) != nil {
		return nil
	}
	result := &PodinfoSummary{}
	for _, cs := range pod.Pod.Status.ContainerStatuses {
		if cs.Name != "test" {
			continue
		}
		for _, sd := range cs.State {
			if code, ok := sd["exitCode"].(float64); ok {
				result.ExitCode = int(code)
			}
			if reason, ok := sd["reason"].(string); ok {
				result.Reason = reason
				if reason == "OOMKilled" {
					result.OOMDetected = true
				}
			}
		}
		for _, sd := range cs.LastState {
			if reason, _ := sd["reason"].(string); reason == "OOMKilled" {
				result.OOMDetected = true
			}
		}
	}
	return result
}

// --- Events summary extraction ---

// EventsSummary summarizes Kubernetes warning events from the build namespace.
type EventsSummary struct {
	CiJobFailed  bool `json:"ci_job_failed"`
	WarningCount int  `json:"warning_count"`
	TotalEvents  int  `json:"total_events"`
}

func extractEventsSummary(data []byte) *EventsSummary {
	var list struct {
		Items []struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil
	}
	result := &EventsSummary{TotalEvents: len(list.Items)}
	for _, item := range list.Items {
		if item.Reason == "CiJobFailed" {
			result.CiJobFailed = true
		}
		if item.Type == "Warning" {
			result.WarningCount++
		}
	}
	return result
}

// --- Pool summary extraction ---

// PoolSummary summarizes managed identity pool state and contention from YAML artifacts.
type PoolSummary struct {
	Total      int      `json:"total"`
	Free       int      `json:"free"`
	Assigned   int      `json:"assigned"`
	Busy       int      `json:"busy"`
	Contention []string `json:"contention,omitempty"`
}

func extractPoolSummary(data []byte) *PoolSummary {
	content := string(data)
	result := &PoolSummary{
		Total:      strings.Count(content, "resourceGroup:"),
		Contention: detectPoolContentionFromString(content),
	}
	inCurrent := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- current:") || trimmed == "current:" {
			inCurrent = true
			continue
		}
		if trimmed == "history:" || trimmed == "resourceGroup:" {
			inCurrent = false
			continue
		}
		if inCurrent && strings.HasPrefix(trimmed, "state:") {
			switch {
			case strings.Contains(trimmed, "free"):
				result.Free++
			case strings.Contains(trimmed, "assigned"):
				result.Assigned++
			case strings.Contains(trimmed, "busy"):
				result.Busy++
			}
		}
	}
	return result
}

func detectPoolContentionFromString(content string) []string {
	var warnings []string
	inCurrent := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- current:") || trimmed == "current:" {
			inCurrent = true
			continue
		}
		if trimmed == "history:" || trimmed == "resourceGroup:" {
			inCurrent = false
			continue
		}
		if inCurrent && strings.HasPrefix(trimmed, "state:") && !strings.Contains(trimmed, "free") {
			warnings = append(warnings, trimmed)
		}
	}
	return warnings
}

// --- Provision summary extraction ---

// ProvisionSummary summarizes infrastructure provisioning step results from JUnit XML.
type ProvisionSummary struct {
	TotalSteps  int                `json:"total_steps"`
	FailedSteps int                `json:"failed_steps"`
	Failures    []ProvisionFailure `json:"failures,omitempty"`
}

// ProvisionFailure records one failed provisioning step.
type ProvisionFailure struct {
	Name    string  `json:"name"`
	TimeSec float64 `json:"time_seconds"`
	Message string  `json:"message"`
}

// extractProvisionSummary parses provision JUnit XML into step counts and failure details.
func extractProvisionSummary(data []byte) *ProvisionSummary {
	suites, err := parseJUnit(data)
	if err != nil {
		return nil
	}
	result := &ProvisionSummary{}
	for _, suite := range suites.Suites {
		for _, tc := range suite.Cases {
			result.TotalSteps++
			if f := tc.effectiveFailure(); f != nil {
				result.FailedSteps++
				msg := f.errorMessage()
				if distilled := extractStepError(msg); distilled != "" {
					msg = distilled
				}
				result.Failures = append(result.Failures, ProvisionFailure{
					Name:    tc.Name,
					TimeSec: tc.Time,
					Message: msg,
				})
			}
		}
	}
	return result
}

// --- Alerts summary extraction ---

// AlertSummary is a deduplicated Azure Monitor alert from a presubmit run.
type AlertSummary struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	State    string `json:"state"`
}

func extractAlertsSummary(data []byte) []AlertSummary {
	var raw struct {
		Alerts []struct {
			Alert struct {
				Name      string `json:"name"`
				Severity  string `json:"severity"`
				State     string `json:"state"`
			} `json:"alert"`
		} `json:"alerts"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	seen := map[string]bool{}
	var result []AlertSummary
	for _, a := range raw.Alerts {
		if seen[a.Alert.Name] {
			continue
		}
		seen[a.Alert.Name] = true
		result = append(result, AlertSummary{
			Name:     a.Alert.Name,
			Severity: a.Alert.Severity,
			State:    a.Alert.State,
		})
	}
	return result
}

// --- Azure summary extraction ---

// AzureTestSummary counts Azure API response errors from a test's azure.log.
type AzureTestSummary struct {
	TestName       string         `json:"test_name"`
	TotalLines     int            `json:"total_lines"`
	ResponseErrors map[string]int `json:"response_errors,omitempty"`
}

var azureErrorCodeRe = regexp.MustCompile(`ERROR CODE:\s*([A-Za-z]+)`)

func extractAzureSummary(data []byte, testName string) *AzureTestSummary {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	result := &AzureTestSummary{
		TestName:   testName,
		TotalLines: len(lines),
	}
	for _, line := range lines {
		if !strings.Contains(line, `"ResponseError"`) {
			continue
		}
		if result.ResponseErrors == nil {
			result.ResponseErrors = map[string]int{}
		}
		code := "unknown"
		if m := azureErrorCodeRe.FindStringSubmatch(line); len(m) > 1 {
			code = m[1]
		}
		result.ResponseErrors[code]++
	}
	return result
}

// --- Run envelope ---

// RunEnvelope holds structural signal extracted from GCS artifacts for a single CI run.
type RunEnvelope struct {
	ExitCode       int                `json:"exit_code"`
	OOM            bool               `json:"oom"`
	ErrorChain     string             `json:"error_chain,omitempty"`
	LeaseWaitSec   float64            `json:"lease_wait_s"`
	PodSchedSec    float64            `json:"pod_sched_s"`
	Steps          []StepTiming       `json:"steps,omitempty"`
	BuildLogErrors []string           `json:"build_log_errors,omitempty"`
	BuildLogSteps  []string           `json:"build_log_steps,omitempty"`
	Alerts         []AlertSummary     `json:"alerts,omitempty"`
	ProvisionFails []ProvisionFailure `json:"provision_failures,omitempty"`
}

func buildRunEnvelope(store *gcs, base, step string, isPresubmit bool) *RunEnvelope {
	env := &RunEnvelope{}

	if data, err := store.artifact(base, "podinfo.json"); err == nil {
		if ps := extractPodinfoSummary(data); ps != nil {
			env.ExitCode = ps.ExitCode
			env.OOM = ps.OOMDetected
		}
	}

	if data, err := store.artifact(base, "artifacts/ci-operator-metrics.json"); err == nil {
		if me := extractMetricsEvents(data); me != nil {
			env.LeaseWaitSec = me.MaxLeaseAcqSec
			env.PodSchedSec = me.MaxPodSchedSec
			for _, ev := range me.Events {
				if !ev.Success && ev.Cause != "" {
					env.ErrorChain = ev.Cause
					break
				}
			}
		}
	}

	if data, err := store.artifact(base, "artifacts/ci-operator-step-graph.json"); err == nil {
		env.Steps = extractStepTimings(data)
		if jd, err := store.artifact(base, "artifacts/junit_operator.xml"); err == nil {
			enrichStepsWithJUnit(env.Steps, jd)
		}
	}

	if data, err := store.artifact(base, "build-log.txt"); err == nil {
		if bl := extractBuildLog(data); bl != nil {
			env.BuildLogErrors = bl.ErrorLines
			env.BuildLogSteps = bl.StepLines
		}
	}

	if isPresubmit {
		alertPath := "artifacts/" + step + "/aro-hcp-gather-observability/artifacts/alerts.json"
		if data, err := store.artifact(base, alertPath); err == nil {
			env.Alerts = extractAlertsSummary(data)
		}
		provPath := "artifacts/" + step + "/aro-hcp-provision-environment/artifacts/junit_entrypoint.xml"
		if data, err := store.artifact(base, provPath); err == nil {
			if ps := extractProvisionSummary(data); ps != nil {
				env.ProvisionFails = ps.Failures
			}
		}
	}

	return env
}

// --- Utility ---
