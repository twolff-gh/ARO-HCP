// Copyright 2025 Microsoft Corporation
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

// Package main implements blast-radius, a tool that scans any Go workspace
// repository and generates a dependency/blast-radius map from its structure.
//
// It answers: "if I change files in directory X, what services, tests,
// modules, and infrastructure are affected?"
//
// Usage:
//
//	blast-radius [--repo-root /path/to/repo] [-o output.yaml]
//
// The tool is fully generic — it works on any repo with a go.work file.
package main

// BlastRadiusFile is the top-level output structure.
type BlastRadiusFile struct {
	Services             ServiceMap            `yaml:"services"`
	ExternalDependencies []ExternalDependency  `yaml:"external_dependencies,omitempty"`
	BlastRadius          map[string]BlastEntry `yaml:"blast_radius"`
	ModuleGraph          ModuleGraph           `yaml:"module_graph"`
	CrossComponentInteractions []Interaction   `yaml:"cross_component_interactions,omitempty"`
}

// ServiceMap groups services by deployment target.
type ServiceMap struct {
	Groups []ServiceGroup `yaml:"groups"`
}

// ServiceGroup is a named collection of services sharing a deployment target.
type ServiceGroup struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description,omitempty"`
	Services    []Service `yaml:"services"`
}

// Service is one deployed component discovered in the repo.
type Service struct {
	Name             string   `yaml:"name"`
	Directory        string   `yaml:"directory"`
	DeployTarget     string   `yaml:"deploy_target,omitempty"`
	InternalPackages []string `yaml:"internal_packages,omitempty"`
	HasDeploy        bool     `yaml:"has_deploy,omitempty"`
	HasPipeline      bool     `yaml:"has_pipeline,omitempty"`
	HasMakefile      bool     `yaml:"has_makefile,omitempty"`
	IsGoModule       bool     `yaml:"is_go_module,omitempty"`
}

// ExternalDependency is an upstream module imported by 2+ workspace modules.
type ExternalDependency struct {
	Name        string   `yaml:"name"`
	Module      string   `yaml:"module"`
	Impact      string   `yaml:"impact"`
	ImportedBy  []string `yaml:"imported_by"`
	SubPackages []string `yaml:"sub_packages,omitempty"`
}

// BlastEntry is one directory's impact metadata.
type BlastEntry struct {
	Impact           string            `yaml:"impact"`
	Scope            string            `yaml:"scope"`
	Reason           string            `yaml:"reason"`
	AffectedServices []string          `yaml:"affected_services,omitempty"`
	AffectedTests    []string          `yaml:"affected_tests,omitempty"`
	DeployTarget     string            `yaml:"deploy_target,omitempty"`
	Children         map[string]string `yaml:"children,omitempty"`
}

// ModuleGraph describes the Go module dependency structure.
type ModuleGraph struct {
	Core     []ModuleEntry `yaml:"core"`
	Interior []ModuleEntry `yaml:"interior"`
	Test     []ModuleEntry `yaml:"test"`
	Leaf     []string      `yaml:"leaf"`
}

// ModuleEntry is one module in the dependency graph.
type ModuleEntry struct {
	Module     string   `yaml:"module"`
	DependsOn  []string `yaml:"depends_on"`
	Dependents []string `yaml:"dependents,omitempty"`
}

// Interaction is a cross-component interaction derived from shared dependencies.
type Interaction struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Implication string `yaml:"implication"`
}
