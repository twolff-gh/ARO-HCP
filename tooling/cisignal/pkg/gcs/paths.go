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

// paths.go maps CI environments and job names to GCS artifact paths within
// the test-platform-results bucket (test results, step-graph, and junit).
package gcs

import (
	"fmt"
	"strings"
)

type envConfig struct {
	step      string // e2e step directory name
	container string // test container name within the step
}

var envConfigs = map[string]envConfig{
	"int":  {step: "integration-e2e-parallel", container: "aro-hcp-test-persistent"},
	"stg":  {step: "stage-e2e-parallel", container: "aro-hcp-test-persistent"},
	"prod": {step: "prod-e2e-parallel", container: "aro-hcp-test-persistent"},
	"dev":  {step: "e2e-parallel", container: "aro-hcp-test-local"},
}

// RunPaths provides GCS artifact paths for one CI run.
// All paths are relative to the bucket root.
type RunPaths struct {
	base      string // "logs/{job}/{run_id}/"
	artifacts string // base + "artifacts/"
	step      string // "integration-e2e-parallel" or resolved variant
	container string // "aro-hcp-test-persistent" or "aro-hcp-test-local"
}

// NewRunPaths constructs artifact paths from a Prow URL and environment.
// When fetcher is non-nil, probes GCS for the actual step directory
// name (handles variant jobs like prod-e2e-parallel-ocp-nightly). Falls
// back to the hardcoded env mapping if listing fails or fetcher is nil.
func NewRunPaths(prowURL, env string, fetcher Fetcher) (RunPaths, error) {
	base := BaseFromProwURL(prowURL)
	if base == "" {
		return RunPaths{}, fmt.Errorf("cannot extract GCS base from URL: %s", prowURL)
	}
	cfg, ok := envConfigs[env]
	if !ok {
		return RunPaths{}, fmt.Errorf("unknown env %q for GCS paths", env)
	}

	step := cfg.step
	artifacts := base + "artifacts/"
	if fetcher != nil {
		if resolved := resolveStep(fetcher, artifacts, cfg.step); resolved != "" {
			step = resolved
		}
	}

	return RunPaths{
		base:      base,
		artifacts: artifacts,
		step:      step,
		container: cfg.container,
	}, nil
}

// resolveStep lists the artifacts/ directory and finds the step
// subdirectory matching the expected name or a variant with a suffix
// (e.g., "prod-e2e-parallel-ocp-nightly"). Returns empty on failure.
func resolveStep(g Fetcher, artifactsPrefix, expectedStep string) string {
	dirs, _, err := g.ListDir(artifactsPrefix)
	if err != nil {
		return ""
	}
	variant := ""
	for _, d := range dirs {
		name := strings.TrimPrefix(d, artifactsPrefix)
		name = strings.TrimSuffix(name, "/")
		if name == expectedStep {
			return name
		}
		if variant == "" && strings.HasPrefix(name, expectedStep+"-") {
			variant = name
		}
	}
	return variant
}

// Container returns the test container name for this run.
func (p RunPaths) Container() string { return p.container }

// BuildLog returns the path to build-log.txt in the test container.
func (p RunPaths) BuildLog() string { return p.artifacts + p.step + "/" + p.container + "/build-log.txt" }

// TestResultsPrefix returns the GCS prefix for extension_test_result files.
func (p RunPaths) TestResultsPrefix() string {
	return p.artifacts + p.step + "/" + p.container + "/artifacts/extension_test_result_e2e_"
}
