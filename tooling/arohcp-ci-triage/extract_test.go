package main

import (
	"testing"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"\x1b[31mERROR\x1b[0m something failed", "ERROR something failed"},
		{"\x1b[1;33mWARNING\x1b[0m", "WARNING"},
		{"no escapes here", "no escapes here"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripANSI(tt.in); got != tt.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExtractBuildLog(t *testing.T) {
	input := []byte("\x1b[31mERROR deployment failed: RoleAssignmentLimitExceeded\x1b[0m\n" +
		"INFO starting step regional/infra\n" +
		"Step regional/infra failed after 12m30s\n" +
		"Step service/cluster succeeded after 5m10s\n" +
		"FATAL cannot continue\n" +
		"line 1\nline 2\nline 3\nline 4\nline 5\n" +
		"line 6\nline 7\nline 8\nline 9\nline 10\n" +
		"line 11\nline 12\nline 13\nline 14\nline 15\n" +
		"3 tests failed\n")

	result := extractBuildLog(input)
	if result == nil {
		t.Fatal("extractBuildLog returned nil")
	}
	if result.FileSizeBytes != len(input) {
		t.Errorf("FileSizeBytes = %d, want %d", result.FileSizeBytes, len(input))
	}
	if len(result.ErrorLines) != 2 {
		t.Errorf("ErrorLines count = %d, want 2 (ERROR + FATAL)", len(result.ErrorLines))
	}
	if len(result.StepLines) != 2 {
		t.Errorf("StepLines count = %d, want 2", len(result.StepLines))
	}
	if result.TestFailCount != 3 {
		t.Errorf("TestFailCount = %d, want 3", result.TestFailCount)
	}
	if len(result.TailLines) == 0 {
		t.Error("TailLines should not be empty")
	}
}

func TestExtractBuildLog_Empty(t *testing.T) {
	result := extractBuildLog([]byte{})
	if result == nil {
		t.Fatal("extractBuildLog returned nil for empty input")
	}
	if result.FileSizeBytes != 0 {
		t.Errorf("FileSizeBytes = %d, want 0", result.FileSizeBytes)
	}
}

func TestExtractMetricsEvents(t *testing.T) {
	input := []byte(`{
		"events": [
			{
				"level": "Info",
				"locator": {"name": "e2e-parallel"},
				"message": {
					"reason": "StepFinished",
					"cause": "",
					"humanMessage": "step finished",
					"annotations": {"duration_seconds": 120.5, "success": true}
				}
			},
			{
				"level": "Error",
				"locator": {"name": "provision-env"},
				"message": {
					"reason": "StepFailed",
					"cause": "timeout",
					"humanMessage": "provision timed out",
					"annotations": {"duration_seconds": 900.0, "success": false}
				}
			}
		],
		"leases": [
			{"acquisition_duration_seconds": 5.2},
			{"acquisition_duration_seconds": 12.8}
		],
		"pods": [
			{"scheduling_latency": 3000000000},
			{"scheduling_latency": 7000000000}
		]
	}`)

	result := extractMetricsEvents(input)
	if result == nil {
		t.Fatal("extractMetricsEvents returned nil")
	}
	if len(result.Events) != 2 {
		t.Fatalf("Events count = %d, want 2", len(result.Events))
	}

	e0 := result.Events[0]
	if e0.StepName != "e2e-parallel" || e0.Level != "Info" || !e0.Success || e0.DurationSec != 120.5 {
		t.Errorf("event 0: got %+v", e0)
	}

	e1 := result.Events[1]
	if e1.StepName != "provision-env" || e1.Cause != "timeout" || e1.Success || e1.DurationSec != 900.0 {
		t.Errorf("event 1: got %+v", e1)
	}

	if result.MaxLeaseAcqSec != 12.8 {
		t.Errorf("MaxLeaseAcqSec = %f, want 12.8", result.MaxLeaseAcqSec)
	}
	if result.MaxPodSchedSec != 7.0 {
		t.Errorf("MaxPodSchedSec = %f, want 7.0", result.MaxPodSchedSec)
	}
}

func TestExtractMetricsEvents_Invalid(t *testing.T) {
	if result := extractMetricsEvents([]byte("not json")); result != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractStepTimings(t *testing.T) {
	input := []byte(`[
		{"name": "build-image", "duration": 120000000000, "failed": false},
		{"name": "provision-env", "duration": 900000000000, "failed": true},
		{"name": "skipped-step", "duration": 0, "failed": false}
	]`)

	steps := extractStepTimings(input)
	if len(steps) != 2 {
		t.Fatalf("step count = %d, want 2 (zero-duration non-failed skipped)", len(steps))
	}
	if steps[0].Name != "build-image" || steps[0].DurationSec != 120.0 || steps[0].Failed {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1].Name != "provision-env" || steps[1].DurationSec != 900.0 || !steps[1].Failed {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestExtractStepTimings_ZeroDurationFailed(t *testing.T) {
	input := []byte(`[{"name": "crashed", "duration": 0, "failed": true}]`)
	steps := extractStepTimings(input)
	if len(steps) != 1 {
		t.Fatalf("zero-duration failed step should be included, got %d steps", len(steps))
	}
}

func TestExtractStepTimings_Invalid(t *testing.T) {
	if steps := extractStepTimings([]byte("not json")); steps != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestEnrichStepsWithJUnit(t *testing.T) {
	steps := []StepTiming{
		{Name: "provision-env", DurationSec: 900, Failed: true},
		{Name: "e2e-parallel", DurationSec: 120, Failed: false},
	}
	junitXML := []byte(`<testsuites>
		<testsuite name="step graph" tests="2" failures="1">
			<testcase name="Run multi-stage test e2e-parallel - provision-env container test" time="900">
				<failure message="TooManyRequests: VMSS creation failed">body</failure>
			</testcase>
			<testcase name="Run multi-stage test e2e-parallel - run-tests container test" time="120"></testcase>
		</testsuite>
	</testsuites>`)

	enrichStepsWithJUnit(steps, junitXML)

	if steps[0].ErrorSnippet != "TooManyRequests: VMSS creation failed" {
		t.Errorf("failed step ErrorSnippet = %q, want TooManyRequests message", steps[0].ErrorSnippet)
	}
}

func TestApplyStepBaseline(t *testing.T) {
	steps := []StepTiming{
		{Name: "build-image", DurationSec: 240},
		{Name: "provision-env", DurationSec: 900},
		{Name: "new-step", DurationSec: 60},
	}
	baselineData := []byte(`[
		{"name": "build-image", "duration": 120000000000, "failed": false},
		{"name": "provision-env", "duration": 300000000000, "failed": false}
	]`)

	applyStepBaseline(steps, baselineData)

	if steps[0].BaselineSec != 120.0 || steps[0].Ratio != 2.0 {
		t.Errorf("build-image: BaselineSec=%f Ratio=%f, want 120.0 and 2.0", steps[0].BaselineSec, steps[0].Ratio)
	}
	if steps[1].BaselineSec != 300.0 || steps[1].Ratio != 3.0 {
		t.Errorf("provision-env: BaselineSec=%f Ratio=%f, want 300.0 and 3.0", steps[1].BaselineSec, steps[1].Ratio)
	}
	if steps[2].BaselineSec != 0 || steps[2].Ratio != 0 {
		t.Errorf("new-step should have zero baseline (no match), got BaselineSec=%f Ratio=%f", steps[2].BaselineSec, steps[2].Ratio)
	}
}

func TestExtractPodinfoSummary(t *testing.T) {
	input := []byte(`{
		"pod": {
			"status": {
				"containerStatuses": [
					{
						"name": "test",
						"state": {
							"terminated": {"exitCode": 2, "reason": "Error"}
						},
						"lastState": {}
					},
					{
						"name": "sidecar",
						"state": {"running": {}}
					}
				]
			}
		}
	}`)

	result := extractPodinfoSummary(input)
	if result == nil {
		t.Fatal("extractPodinfoSummary returned nil")
	}
	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
	if result.Reason != "Error" {
		t.Errorf("Reason = %q, want Error", result.Reason)
	}
	if result.OOMDetected {
		t.Error("OOMDetected should be false for non-OOM exit")
	}
}

func TestExtractPodinfoSummary_OOM(t *testing.T) {
	input := []byte(`{
		"pod": {
			"status": {
				"containerStatuses": [{
					"name": "test",
					"state": {"terminated": {"exitCode": 137, "reason": "OOMKilled"}},
					"lastState": {}
				}]
			}
		}
	}`)

	result := extractPodinfoSummary(input)
	if result == nil {
		t.Fatal("returned nil")
	}
	if !result.OOMDetected {
		t.Error("OOMDetected should be true")
	}
}

func TestExtractPodinfoSummary_OOMLastState(t *testing.T) {
	input := []byte(`{
		"pod": {
			"status": {
				"containerStatuses": [{
					"name": "test",
					"state": {"terminated": {"exitCode": 2, "reason": "Error"}},
					"lastState": {"terminated": {"exitCode": 137, "reason": "OOMKilled"}}
				}]
			}
		}
	}`)

	result := extractPodinfoSummary(input)
	if result == nil {
		t.Fatal("returned nil")
	}
	if !result.OOMDetected {
		t.Error("OOMDetected should be true from lastState")
	}
}

func TestExtractEventsSummary(t *testing.T) {
	input := []byte(`{
		"items": [
			{"type": "Normal", "reason": "Scheduled"},
			{"type": "Warning", "reason": "BackOff"},
			{"type": "Warning", "reason": "CiJobFailed"},
			{"type": "Normal", "reason": "Started"}
		]
	}`)

	result := extractEventsSummary(input)
	if result == nil {
		t.Fatal("returned nil")
	}
	if result.TotalEvents != 4 {
		t.Errorf("TotalEvents = %d, want 4", result.TotalEvents)
	}
	if result.WarningCount != 2 {
		t.Errorf("WarningCount = %d, want 2", result.WarningCount)
	}
	if !result.CiJobFailed {
		t.Error("CiJobFailed should be true")
	}
}

func TestExtractEventsSummary_Invalid(t *testing.T) {
	if result := extractEventsSummary([]byte("not json")); result != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractPoolSummary(t *testing.T) {
	input := []byte(`containers:
  - resourceGroup: rg-1
    current:
      state: free
    history:
      - state: assigned
  - resourceGroup: rg-2
    current:
      state: assigned
    history:
      - state: free
  - resourceGroup: rg-3
    current:
      state: busy
    history:
      - state: free
  - resourceGroup: rg-4
    current:
      state: free
    history:
      - state: assigned
`)

	result := extractPoolSummary(input)
	if result.Total != 4 {
		t.Errorf("Total = %d, want 4", result.Total)
	}
	if result.Free != 2 {
		t.Errorf("Free = %d, want 2 (current only, not history)", result.Free)
	}
	if result.Assigned != 1 {
		t.Errorf("Assigned = %d, want 1 (current only)", result.Assigned)
	}
	if result.Busy != 1 {
		t.Errorf("Busy = %d, want 1 (current only)", result.Busy)
	}
}

func TestExtractProvisionSummary(t *testing.T) {
	input := []byte(`<testsuites>
		<testsuite name="provision" tests="3" failures="1">
			<testcase name="create-vnet" time="30.5"></testcase>
			<testcase name="create-cluster" time="600.0">
				<failure message="TooManyRequests">quota exceeded</failure>
			</testcase>
			<testcase name="configure-dns" time="15.0"></testcase>
		</testsuite>
	</testsuites>`)

	result := extractProvisionSummary(input)
	if result == nil {
		t.Fatal("returned nil")
	}
	if result.TotalSteps != 3 {
		t.Errorf("TotalSteps = %d, want 3", result.TotalSteps)
	}
	if result.FailedSteps != 1 {
		t.Errorf("FailedSteps = %d, want 1", result.FailedSteps)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("Failures count = %d, want 1", len(result.Failures))
	}
	f := result.Failures[0]
	if f.Name != "create-cluster" {
		t.Errorf("failure Name = %q, want create-cluster", f.Name)
	}
	if f.TimeSec != 600.0 {
		t.Errorf("failure TimeSec = %f, want 600.0", f.TimeSec)
	}
	if f.Message != "TooManyRequests" {
		t.Errorf("failure Message = %q, want TooManyRequests", f.Message)
	}
}

func TestExtractAlertsSummary(t *testing.T) {
	input := []byte(`{
		"alerts": [
			{"alert": {"name": "KubeVersionMismatch", "severity": "warning", "state": "firing"}},
			{"alert": {"name": "KubeVersionMismatch", "severity": "warning", "state": "firing"}},
			{"alert": {"name": "HighMemoryUsage", "severity": "critical", "state": "pending"}}
		]
	}`)

	result := extractAlertsSummary(input)
	if len(result) != 2 {
		t.Fatalf("alert count = %d, want 2 (deduplicated)", len(result))
	}
	if result[0].Name != "KubeVersionMismatch" || result[0].Severity != "warning" || result[0].State != "firing" {
		t.Errorf("alert 0: got %+v", result[0])
	}
	if result[1].Name != "HighMemoryUsage" || result[1].Severity != "critical" {
		t.Errorf("alert 1: got %+v", result[1])
	}
}

func TestExtractAlertsSummary_Invalid(t *testing.T) {
	if result := extractAlertsSummary([]byte("not json")); result != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestExtractAzureSummary(t *testing.T) {
	input := []byte(
		`{"time":"2026-04-22T17:00:00Z","level":"INFO","event":"Request","msg":"GET /resource"}` + "\n" +
			`{"time":"2026-04-22T17:01:00Z","level":"ERROR","event":"ResponseError","msg":"ERROR CODE: RoleAssignmentLimitExceeded"}` + "\n" +
			`{"time":"2026-04-22T17:02:00Z","level":"ERROR","event":"ResponseError","msg":"ERROR CODE: RoleAssignmentLimitExceeded"}` + "\n" +
			`{"time":"2026-04-22T17:03:00Z","level":"ERROR","event":"ResponseError","msg":"ERROR CODE: QuotaExceeded"}`)

	result := extractAzureSummary(input, "test-create-cluster")
	if result == nil {
		t.Fatal("returned nil")
	}
	if result.TestName != "test-create-cluster" {
		t.Errorf("TestName = %q", result.TestName)
	}
	if result.TotalLines != 4 {
		t.Errorf("TotalLines = %d, want 4", result.TotalLines)
	}
	if result.ResponseErrors["RoleAssignmentLimitExceeded"] != 2 {
		t.Errorf("RoleAssignmentLimitExceeded count = %d, want 2", result.ResponseErrors["RoleAssignmentLimitExceeded"])
	}
	if result.ResponseErrors["QuotaExceeded"] != 1 {
		t.Errorf("QuotaExceeded count = %d, want 1", result.ResponseErrors["QuotaExceeded"])
	}
}

func TestExtractAzureSummary_NoErrors(t *testing.T) {
	input := []byte(`{"time":"2026-04-22T17:00:00Z","level":"INFO","event":"Request","msg":"GET /resource"}` + "\n" +
		`{"time":"2026-04-22T17:01:00Z","level":"INFO","event":"Response","msg":"200 OK"}`)

	result := extractAzureSummary(input, "test-clean")
	if result == nil {
		t.Fatal("returned nil")
	}
	if result.TotalLines != 2 {
		t.Errorf("TotalLines = %d, want 2", result.TotalLines)
	}
	if result.ResponseErrors != nil {
		t.Errorf("ResponseErrors should be nil, got %v", result.ResponseErrors)
	}
}

func TestExtractTestLinks(t *testing.T) {
	html := `<html>
	<h2>Test: create-cluster</h2>
	<p>az group show --resource-group rg-create-cluster-abc123</p>
	<p>Kusto: hcp-underlay-int.eastus2.kusto.windows.net</p>
	<h2>Test: delete-cluster</h2>
	<p>az group show --resource-group rg-delete-cluster-def456</p>
	<p>az group show --resource-group rg-create-cluster-abc123</p>
	</html>`

	links := extractTestLinks(html)
	if len(links) != 2 {
		t.Fatalf("link count = %d, want 2 (deduplicated)", len(links))
	}

	found := map[string]TestLink{}
	for _, l := range links {
		found[l.ResourceGroup] = l
	}

	rg1, ok := found["rg-create-cluster-abc123"]
	if !ok {
		t.Fatal("missing rg-create-cluster-abc123")
	}
	if rg1.KustoCluster != "hcp-underlay-int.eastus2.kusto.windows.net" {
		t.Errorf("KustoCluster = %q", rg1.KustoCluster)
	}

	rg2, ok := found["rg-delete-cluster-def456"]
	if !ok {
		t.Fatal("missing rg-delete-cluster-def456")
	}
	if rg2.KustoCluster != "" {
		t.Errorf("second section has no kusto, got %q", rg2.KustoCluster)
	}
}

func TestExtractAzureSummary_LROStates(t *testing.T) {
	input := []byte(`{"time":"2026-04-28T12:11:15Z","level":"INFO","event":"Retry","msg":"response 200"}
{"time":"2026-04-28T12:11:15Z","level":"INFO","event":"LongRunningOperation","msg":"BEGIN PollUntilDone() for *async.Poller[github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources.DeploymentsClientCreateOrUpdateResponse]"}
{"time":"2026-04-28T12:11:16Z","level":"INFO","event":"LongRunningOperation","msg":"State Accepted"}
{"time":"2026-04-28T12:11:26Z","level":"INFO","event":"LongRunningOperation","msg":"State Running"}
{"time":"2026-04-28T12:11:36Z","level":"INFO","event":"LongRunningOperation","msg":"State Succeeded"}
{"time":"2026-04-28T12:11:36Z","level":"INFO","event":"LongRunningOperation","msg":"END PollUntilDone()"}
{"time":"2026-04-28T12:12:26Z","level":"INFO","event":"LongRunningOperation","msg":"BEGIN PollUntilDone() for *async.Poller[github.com/Azure/ARO-HCP/test/sdk/resourcemanager/redhatopenshifthcp/armredhatopenshifthcp.HcpOpenShiftClustersClientCreateOrUpdateResponse]"}
{"time":"2026-04-28T12:12:27Z","level":"INFO","event":"LongRunningOperation","msg":"State Accepted"}
{"time":"2026-04-28T12:12:37Z","level":"INFO","event":"LongRunningOperation","msg":"State Accepted"}
{"time":"2026-04-28T12:57:24Z","level":"INFO","event":"LongRunningOperation","msg":"END PollUntilDone()"}
{"time":"2026-04-28T12:11:11Z","level":"INFO","event":"ResponseError","msg":"ERROR CODE: Conflict"}`)

	summary := extractAzureSummary(input, "test-cluster-create")

	if summary.TotalLines != 11 {
		t.Errorf("TotalLines = %d, want 11", summary.TotalLines)
	}
	if len(summary.ResponseErrors) != 1 || summary.ResponseErrors["Conflict"] != 1 {
		t.Errorf("ResponseErrors = %v, want {Conflict:1}", summary.ResponseErrors)
	}
	if summary.LROStates["Accepted"] != 3 {
		t.Errorf("LROStates[Accepted] = %d, want 3", summary.LROStates["Accepted"])
	}
	if summary.LROStates["Running"] != 1 {
		t.Errorf("LROStates[Running] = %d, want 1", summary.LROStates["Running"])
	}
	if summary.LROStates["Succeeded"] != 1 {
		t.Errorf("LROStates[Succeeded] = %d, want 1", summary.LROStates["Succeeded"])
	}
	if len(summary.LROPollerTypes) != 2 {
		t.Fatalf("LROPollerTypes len = %d, want 2", len(summary.LROPollerTypes))
	}
	if summary.LROPollerTypes[0] != "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources.DeploymentsClientCreateOrUpdateResponse" {
		t.Errorf("LROPollerTypes[0] = %s", summary.LROPollerTypes[0])
	}
	if summary.LROPollerTypes[1] != "github.com/Azure/ARO-HCP/test/sdk/resourcemanager/redhatopenshifthcp/armredhatopenshifthcp.HcpOpenShiftClustersClientCreateOrUpdateResponse" {
		t.Errorf("LROPollerTypes[1] = %s", summary.LROPollerTypes[1])
	}
}

func TestClassifyLRO(t *testing.T) {
	tests := []struct {
		desc  string
		azure []AzureTestSummary
		want  string
	}{
		{
			"accepted stuck — ClassicStorage pattern",
			[]AzureTestSummary{
				{LROStates: map[string]int{"Accepted": 258, "Running": 5, "Succeeded": 2}},
				{LROStates: map[string]int{"Accepted": 259, "Running": 3, "Succeeded": 2}},
			},
			"accepted_stuck",
		},
		{
			"provisioning stuck — PROD/nightly pattern",
			[]AzureTestSummary{
				{LROStates: map[string]int{"Accepted": 28, "Provisioning": 232, "Running": 2, "Succeeded": 2}},
				{LROStates: map[string]int{"Accepted": 27, "Provisioning": 232, "Running": 3, "Succeeded": 2}},
			},
			"provisioning_stuck",
		},
		{
			"healthy — normal run",
			[]AzureTestSummary{
				{LROStates: map[string]int{"Accepted": 14, "Provisioning": 84, "Running": 4, "Succeeded": 4}},
			},
			"",
		},
		{
			"no LRO data",
			[]AzureTestSummary{
				{ResponseErrors: map[string]int{"Conflict": 1}},
			},
			"",
		},
		{
			"empty",
			nil,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := classifyLRO(tt.azure); got != tt.want {
				t.Errorf("classifyLRO() = %q, want %q", got, tt.want)
			}
		})
	}
}
