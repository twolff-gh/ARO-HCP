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

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Scanner scans a Go workspace repository and produces a BlastRadiusFile.
type Scanner struct {
	repoRoot  string
	goModBase string // base module path (e.g. "github.com/Azure/ARO-HCP")

	modules  map[string]moduleInfo // short name -> info
	depGraph map[string][]string   // module -> local modules it depends on
	revGraph map[string][]string   // module -> local modules that depend on it

	// Config: patterns in pipeline.yaml/Makefile that indicate deployment targets.
	// Key = target name, Value = list of string patterns to search for.
	// If empty, no deployment target classification is performed.
	deployTargetPatterns map[string][]string

	// Config: infrastructure directories to scan for templates/modules.
	// Default: ["dev-infrastructure"] if it exists.
	infraDirs []string
}

type moduleInfo struct {
	shortName    string
	fullPath     string
	dir          string
	localDeps    []string
	externalReqs map[string]bool // all external require paths from go.mod
}

// NewScanner creates a Scanner rooted at the given repo directory.
func NewScanner(repoRoot string) *Scanner {
	return &Scanner{
		repoRoot: repoRoot,
		modules:  make(map[string]moduleInfo),
		depGraph: make(map[string][]string),
		revGraph: make(map[string][]string),
	}
}

// SetDeployTargetPatterns configures how to classify services into deployment
// targets. Each key is a target name (e.g., "svc", "mgmt"), and the value is
// a list of string patterns to search for in pipeline.yaml and Makefile files.
func (s *Scanner) SetDeployTargetPatterns(patterns map[string][]string) {
	s.deployTargetPatterns = patterns
}

// Scan performs a full repo scan and returns the blast-radius data.
func (s *Scanner) Scan() (*BlastRadiusFile, error) {
	if err := s.parseGoWork(); err != nil {
		return nil, fmt.Errorf("parsing go.work: %w", err)
	}
	if err := s.parseAllGoMods(); err != nil {
		return nil, fmt.Errorf("parsing go.mod files: %w", err)
	}
	s.buildReverseGraph()
	s.detectInfraDirs()

	result := &BlastRadiusFile{}
	result.ModuleGraph = s.buildModuleGraph()
	result.Services = s.discoverServices()
	result.ExternalDependencies = s.detectExternalDeps()
	result.BlastRadius = s.computeBlastRadius()
	result.CrossComponentInteractions = s.deriveInteractions()

	return result, nil
}

// ---------------------------------------------------------------------------
// go.work parsing
// ---------------------------------------------------------------------------

func (s *Scanner) parseGoWork() error {
	path := filepath.Join(s.repoRoot, "go.work")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening go.work: %w", err)
	}
	defer f.Close()

	inUse := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "use (" {
			inUse = true
			continue
		}
		if line == ")" {
			inUse = false
			continue
		}
		if rest, ok := strings.CutPrefix(line, "use "); ok && !strings.Contains(line, "(") {
			s.addModule(strings.TrimSpace(rest))
			continue
		}
		if inUse {
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			s.addModule(line)
		}
	}
	return sc.Err()
}

func (s *Scanner) addModule(dir string) {
	dir = strings.Trim(dir, "./")
	if dir == "" {
		dir = "."
	}
	s.modules[dir] = moduleInfo{
		shortName: dir,
		dir:       filepath.Join(s.repoRoot, dir),
	}
}

// ---------------------------------------------------------------------------
// go.mod parsing — two-pass: collect requires, then match replaces
// ---------------------------------------------------------------------------

var replaceRE = regexp.MustCompile(`^([\w./-]+)\s+=>\s+(\.\..*)$`)

func (s *Scanner) parseAllGoMods() error {
	for name, mod := range s.modules {
		gomodPath := filepath.Join(mod.dir, "go.mod")
		info, err := parseGoMod(gomodPath, mod, s.repoRoot, s.modules)
		if err != nil {
			return fmt.Errorf("parsing %s/go.mod: %w", name, err)
		}
		// Detect base module path from the shortest non-tooling module
		if s.goModBase == "" && !strings.Contains(info.fullPath, "/tooling/") {
			parts := strings.Split(info.fullPath, "/")
			if len(parts) >= 3 {
				s.goModBase = strings.Join(parts[:3], "/")
			}
		}
		s.modules[name] = info
		s.depGraph[name] = info.localDeps
	}
	return nil
}

func parseGoMod(path string, info moduleInfo, repoRoot string, allModules map[string]moduleInfo) (moduleInfo, error) {
	info.externalReqs = make(map[string]bool)

	content, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	lines := strings.Split(string(content), "\n")

	// Pass 1: collect all required module paths.
	// Track direct vs indirect separately — indirect deps are transitive noise.
	required := make(map[string]bool)   // all requires (for replace matching)
	directReqs := make(map[string]bool) // only direct requires (for external dep detection)
	inRequire := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "require (" {
			inRequire = true
			continue
		}
		if line == ")" {
			inRequire = false
			continue
		}
		if rest, ok := strings.CutPrefix(line, "require "); ok && !strings.Contains(line, "(") {
			parts := strings.Fields(rest)
			if len(parts) >= 1 {
				required[parts[0]] = true
				if !strings.Contains(line, "// indirect") {
					directReqs[parts[0]] = true
				}
			}
			continue
		}
		if inRequire {
			parts := strings.Fields(line)
			if len(parts) >= 1 && !strings.HasPrefix(parts[0], "//") {
				required[parts[0]] = true
				if !strings.Contains(raw, "// indirect") {
					directReqs[parts[0]] = true
				}
			}
		}
	}

	// Pass 2: module path + replace directives
	inReplace := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)

		if modPath, ok := strings.CutPrefix(line, "module "); ok {
			info.fullPath = modPath
			continue
		}
		if line == "replace (" {
			inReplace = true
			continue
		}
		if line == ")" {
			inReplace = false
			continue
		}
		if rest, ok := strings.CutPrefix(line, "replace "); ok && !strings.Contains(line, "(") {
			processReplace(rest, &info, required, repoRoot, allModules)
			continue
		}
		if inReplace {
			processReplace(line, &info, required, repoRoot, allModules)
		}
	}

	// Collect direct (non-indirect) external requires.
	// Skip anything that looks like a local workspace module (matches a known
	// module fullPath OR shares the same base module path prefix).
	for reqPath := range directReqs {
		isLocal := false
		for _, mod := range allModules {
			if mod.fullPath != "" && reqPath == mod.fullPath {
				isLocal = true
				break
			}
		}
		// Also skip if this is a sub-path of our own repo (workspace modules
		// that haven't had their fullPath set yet during parsing)
		if !isLocal && info.fullPath != "" {
			parts := strings.Split(info.fullPath, "/")
			if len(parts) >= 3 {
				base := strings.Join(parts[:3], "/")
				if strings.HasPrefix(reqPath, base+"/") {
					isLocal = true
				}
			}
		}
		if !isLocal {
			info.externalReqs[reqPath] = true
		}
	}

	return info, nil
}

func processReplace(line string, info *moduleInfo, required map[string]bool, repoRoot string, allModules map[string]moduleInfo) {
	m := replaceRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return
	}
	modulePath := m[1]
	relPath := m[2]

	// Only count as real dependency if also in require block
	if !required[modulePath] {
		return
	}

	absPath := filepath.Join(info.dir, relPath)
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == "" {
		rel = "."
	}
	if _, ok := allModules[rel]; ok {
		info.localDeps = append(info.localDeps, rel)
	}
}

func (s *Scanner) buildReverseGraph() {
	for mod, deps := range s.depGraph {
		for _, dep := range deps {
			s.revGraph[dep] = append(s.revGraph[dep], mod)
		}
	}
}

// ---------------------------------------------------------------------------
// Module graph classification
// ---------------------------------------------------------------------------

func (s *Scanner) buildModuleGraph() ModuleGraph {
	mg := ModuleGraph{}

	for name := range s.modules {
		deps := s.depGraph[name]
		revDeps := s.revGraph[name]

		entry := ModuleEntry{
			Module:    name,
			DependsOn: deps,
		}
		if len(revDeps) > 0 {
			sorted := make([]string, len(revDeps))
			copy(sorted, revDeps)
			sort.Strings(sorted)
			entry.Dependents = sorted
		}

		switch {
		case len(revDeps) >= 3:
			mg.Core = append(mg.Core, entry)
		case strings.HasPrefix(name, "test"):
			mg.Test = append(mg.Test, entry)
		case len(revDeps) == 0:
			mg.Leaf = append(mg.Leaf, name)
		default:
			mg.Interior = append(mg.Interior, entry)
		}
	}

	sort.Strings(mg.Leaf)
	sort.Slice(mg.Core, func(i, j int) bool { return mg.Core[i].Module < mg.Core[j].Module })
	sort.Slice(mg.Interior, func(i, j int) bool { return mg.Interior[i].Module < mg.Interior[j].Module })
	sort.Slice(mg.Test, func(i, j int) bool { return mg.Test[i].Module < mg.Test[j].Module })

	return mg
}

// ---------------------------------------------------------------------------
// Service discovery
// ---------------------------------------------------------------------------

func (s *Scanner) discoverServices() ServiceMap {
	sm := ServiceMap{}
	targetServices := make(map[string][]Service) // deploy target -> services

	entries, err := os.ReadDir(s.repoRoot)
	if err != nil {
		return sm
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") ||
			entry.Name() == "vendor" || entry.Name() == "node_modules" || entry.Name() == "testdata" {
			continue
		}
		name := entry.Name()
		dir := filepath.Join(s.repoRoot, name)

		svc := s.probeService(name, dir)
		if svc != nil {
			target := s.resolveDeployTarget(svc)
			targetServices[target] = append(targetServices[target], *svc)
		}

		// Check subdirectories (e.g., maestro/server, observability/tracing)
		s.probeSubdirs(name, dir, targetServices)
	}

	// Group services by deploy target
	targets := make([]string, 0, len(targetServices))
	for t := range targetServices {
		targets = append(targets, t)
	}
	sort.Strings(targets)

	for _, target := range targets {
		services := targetServices[target]
		sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
		sm.Groups = append(sm.Groups, ServiceGroup{
			Name:     target,
			Services: services,
		})
	}

	return sm
}

func (s *Scanner) probeService(name, dir string) *Service {
	hasDeploy := dirExists(filepath.Join(dir, "deploy"))
	hasPipeline := fileExists(filepath.Join(dir, "pipeline.yaml"))
	hasMakefile := fileExists(filepath.Join(dir, "Makefile"))

	if !hasDeploy && !hasPipeline {
		return nil
	}

	_, isGoMod := s.modules[name]

	svc := &Service{
		Name:        name,
		Directory:   name + "/",
		HasDeploy:   hasDeploy,
		HasPipeline: hasPipeline,
		HasMakefile: hasMakefile,
		IsGoModule:  isGoMod,
	}

	if isGoMod {
		svc.InternalPackages = s.scanInternalImports(name)
	}

	return svc
}

func (s *Scanner) probeSubdirs(parentName, parentDir string, targetServices map[string][]Service) {
	subEntries, err := os.ReadDir(parentDir)
	if err != nil {
		return
	}
	for _, sub := range subEntries {
		if !sub.IsDir() || sub.Name() == "testdata" || strings.HasPrefix(sub.Name(), ".") {
			continue
		}
		subName := parentName + "/" + sub.Name()
		subDir := filepath.Join(parentDir, sub.Name())

		svc := s.probeService(subName, subDir)
		if svc != nil {
			target := s.resolveDeployTarget(svc)
			targetServices[target] = append(targetServices[target], *svc)
			continue
		}

		// One more level deep
		deepEntries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}
		for _, deep := range deepEntries {
			if !deep.IsDir() || deep.Name() == "testdata" || strings.HasPrefix(deep.Name(), ".") {
				continue
			}
			deepName := subName + "/" + deep.Name()
			deepDir := filepath.Join(subDir, deep.Name())
			deepSvc := s.probeService(deepName, deepDir)
			if deepSvc != nil {
				target := s.resolveDeployTarget(deepSvc)
				targetServices[target] = append(targetServices[target], *deepSvc)
			}
		}
	}
}

func (s *Scanner) resolveDeployTarget(svc *Service) string {
	if len(s.deployTargetPatterns) == 0 {
		return "default"
	}

	// Check root Makefile first
	rootMakefile := filepath.Join(s.repoRoot, "Makefile")
	if content, err := os.ReadFile(rootMakefile); err == nil {
		makefileName := strings.ReplaceAll(svc.Name, "/", ".")
		baseName := filepath.Base(svc.Name)
		text := string(content)
		matches := make(map[string]bool)

		for target, patterns := range s.deployTargetPatterns {
			for _, line := range strings.Split(text, "\n") {
				for _, pat := range patterns {
					if strings.Contains(line, pat) &&
						(strings.Contains(line, makefileName) || strings.Contains(line, baseName)) {
						matches[target] = true
					}
				}
			}
		}

		if len(matches) > 1 {
			return "multi"
		}
		for t := range matches {
			return t
		}
	}

	// Check service's own pipeline.yaml and Makefile
	for _, filename := range []string{"pipeline.yaml", "Makefile"} {
		path := filepath.Join(s.repoRoot, strings.TrimSuffix(svc.Directory, "/"), filename)
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(content)
		matches := make(map[string]bool)

		for target, patterns := range s.deployTargetPatterns {
			for _, pat := range patterns {
				if strings.Contains(text, pat) {
					matches[target] = true
				}
			}
		}

		if len(matches) > 1 {
			return "multi"
		}
		for t := range matches {
			return t
		}
	}

	return "unclassified"
}

// ---------------------------------------------------------------------------
// Internal package import scanning
// ---------------------------------------------------------------------------

func (s *Scanner) scanInternalImports(moduleName string) []string {
	modInfo := s.modules[moduleName]
	if s.goModBase == "" {
		return nil
	}

	internalPrefix := s.goModBase + "/internal/"
	packageSet := make(map[string]bool)

	filepath.Walk(modInfo.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "vendor" || base == "testdata" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		extractImports(path, internalPrefix, packageSet)
		return nil
	})

	result := make([]string, 0, len(packageSet))
	for pkg := range packageSet {
		result = append(result, pkg)
	}
	sort.Strings(result)
	return result
}

var importRE = regexp.MustCompile(`"([^"]+)"`)

func extractImports(path, prefix string, packageSet map[string]bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	inImport := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "import (" {
			inImport = true
			continue
		}
		if inImport && line == ")" {
			inImport = false
			continue
		}
		if inImport || strings.HasPrefix(line, "import ") {
			m := importRE.FindStringSubmatch(line)
			if m != nil {
				if rest, ok := strings.CutPrefix(m[1], prefix); ok {
					packageSet[rest] = true
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// External dependency detection — fully automatic
// ---------------------------------------------------------------------------

func (s *Scanner) detectExternalDeps() []ExternalDependency {
	// Count how many workspace SERVICE modules (non-test, non-tooling) require
	// each external module. Only report repos imported by 3+ service modules —
	// those are the "shared external dependencies" that create cross-service coupling.
	// Common Go infrastructure libs (k8s.io, golang.org, etc.) are filtered out.
	extCount := make(map[string][]string) // external module path -> importing workspace modules

	for modName, mod := range s.modules {
		for reqPath := range mod.externalReqs {
			extCount[reqPath] = append(extCount[reqPath], modName)
		}
	}

	// Group by org/repo prefix (e.g., github.com/openshift/hypershift/api/v1beta1 → github.com/openshift/hypershift)
	type depGroup struct {
		prefix     string
		modules    map[string]bool // workspace modules that import it
		subModules []string        // specific external module paths
	}
	groups := make(map[string]*depGroup)

	// Common infrastructure module prefixes to exclude — these are generic Go
	// plumbing, not domain-specific shared dependencies worth tracking.
	infraPrefixes := []string{
		"k8s.io/", "sigs.k8s.io/", "golang.org/", "google.golang.org/",
		"go.opentelemetry.io/", "go.uber.org/", "cel.dev/",
		"github.com/stretchr/", "github.com/go-logr/", "github.com/spf13/",
		"github.com/prometheus/", "github.com/grpc-ecosystem/",
	}

	for extPath, importers := range extCount {
		// Skip common infrastructure deps
		skip := false
		for _, p := range infraPrefixes {
			if strings.HasPrefix(extPath, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Derive the repo prefix (first 3 path segments for github.com/org/repo)
		parts := strings.Split(extPath, "/")
		prefix := extPath
		if len(parts) >= 3 && (parts[0] == "github.com" || parts[0] == "gitlab.com" || parts[0] == "bitbucket.org") {
			prefix = strings.Join(parts[:3], "/")
		}

		if groups[prefix] == nil {
			groups[prefix] = &depGroup{prefix: prefix, modules: make(map[string]bool)}
		}
		g := groups[prefix]
		for _, imp := range importers {
			g.modules[imp] = true
		}
		if extPath != prefix {
			g.subModules = append(g.subModules, extPath)
		}
	}

	var deps []ExternalDependency
	for prefix, g := range groups {
		// Only report deps shared by 3+ service modules (filters noise)
		serviceModules := filterNonTest(filterNonTooling(mapKeys(g.modules)))
		if len(serviceModules) < 2 {
			continue
		}

		importedBy := make([]string, 0, len(g.modules))
		for m := range g.modules {
			importedBy = append(importedBy, m)
		}
		sort.Strings(importedBy)
		sort.Strings(g.subModules)

		// Scan source files for specific sub-packages used
		subPkgs := s.scanExternalSubPackages(prefix, importedBy)

		impact := "high"
		if len(importedBy) >= 4 {
			impact = "critical"
		}

		// Derive short name from prefix
		nameParts := strings.Split(prefix, "/")
		shortName := nameParts[len(nameParts)-1]

		deps = append(deps, ExternalDependency{
			Name:        shortName,
			Module:      prefix,
			Impact:      impact,
			ImportedBy:  importedBy,
			SubPackages: subPkgs,
		})
	}

	sort.Slice(deps, func(i, j int) bool { return deps[i].Module < deps[j].Module })
	return deps
}

func (s *Scanner) scanExternalSubPackages(prefix string, importedBy []string) []string {
	pkgSet := make(map[string]bool)

	for _, modName := range importedBy {
		mod, ok := s.modules[modName]
		if !ok {
			continue
		}
		filepath.Walk(mod.dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				if info != nil && info.IsDir() {
					base := filepath.Base(path)
					if base == "vendor" || base == "testdata" || strings.HasPrefix(base, ".") {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()

			sc := bufio.NewScanner(f)
			inImport := false
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "import (" {
					inImport = true
					continue
				}
				if inImport && line == ")" {
					inImport = false
					continue
				}
				if inImport || strings.HasPrefix(line, "import ") {
					m := importRE.FindStringSubmatch(line)
					if m != nil && strings.HasPrefix(m[1], prefix) {
						sub := strings.TrimPrefix(m[1], prefix)
						sub = strings.TrimPrefix(sub, "/")
						if sub == "" {
							sub = "(root)"
						}
						pkgSet[sub] = true
					}
				}
			}
			return nil
		})
	}

	result := make([]string, 0, len(pkgSet))
	for pkg := range pkgSet {
		result = append(result, pkg)
	}
	sort.Strings(result)
	return result
}

// ---------------------------------------------------------------------------
// Blast radius computation
// ---------------------------------------------------------------------------

func (s *Scanner) computeBlastRadius() map[string]BlastEntry {
	br := make(map[string]BlastEntry)

	// Go modules
	for name := range s.modules {
		if name == "." {
			continue
		}
		dir := name + "/"
		revDeps := s.revGraph[name]
		depCount := len(revDeps)

		impact := s.classifyImpact(name, depCount)
		scope := s.computeScope(name, revDeps)
		reason := s.computeReason(name, depCount, revDeps)

		entry := BlastEntry{
			Impact: impact,
			Scope:  scope,
			Reason: reason,
		}
		if len(revDeps) > 0 {
			entry.AffectedServices = filterNonTest(revDeps)
			entry.AffectedTests = filterTest(revDeps)
		}

		target := s.detectDeployTargetForDir(name)
		if target != "" {
			entry.DeployTarget = target
		}

		br[dir] = entry
	}

	// Infrastructure directories
	s.addInfraEntries(br)

	// Non-module service directories
	s.addNonModuleServices(br)

	// Config directory
	if dirExists(filepath.Join(s.repoRoot, "config")) {
		br["config/"] = BlastEntry{
			Impact: "high",
			Scope:  "all environments",
			Reason: "Central configuration — affects how all services are configured",
		}
	}

	return br
}

func (s *Scanner) classifyImpact(name string, depCount int) string {
	switch {
	case depCount >= 5:
		return "critical"
	case depCount >= 3:
		return "high"
	case depCount >= 2:
		return "medium-high"
	case depCount == 1:
		return "medium"
	default:
		dir := filepath.Join(s.repoRoot, name)
		if dirExists(filepath.Join(dir, "deploy")) || fileExists(filepath.Join(dir, "pipeline.yaml")) {
			return "low"
		}
		return "minimal"
	}
}

func (s *Scanner) computeScope(name string, revDeps []string) string {
	if len(revDeps) == 0 {
		if dirExists(filepath.Join(s.repoRoot, name, "deploy")) {
			return name + " service only"
		}
		return "developer tooling only — does not affect deployed services"
	}
	if len(revDeps) >= 4 {
		return "all services"
	}
	sorted := make([]string, len(revDeps))
	copy(sorted, revDeps)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}

func (s *Scanner) computeReason(name string, depCount int, revDeps []string) string {
	if depCount == 0 {
		if dirExists(filepath.Join(s.repoRoot, name, "deploy")) {
			return "Deployed service — standalone module"
		}
		return "Leaf module — no other modules depend on this"
	}
	sorted := make([]string, len(revDeps))
	copy(sorted, revDeps)
	sort.Strings(sorted)
	return fmt.Sprintf("Imported by %d modules: %s", depCount, strings.Join(sorted, ", "))
}

func (s *Scanner) detectDeployTargetForDir(name string) string {
	if len(s.deployTargetPatterns) == 0 {
		return ""
	}
	svc := &Service{Name: name, Directory: name + "/"}
	target := s.resolveDeployTarget(svc)
	if target == "unclassified" || target == "default" {
		return ""
	}
	return target
}

func filterNonTest(modules []string) []string {
	var result []string
	for _, m := range modules {
		if !strings.HasPrefix(m, "test") {
			result = append(result, m)
		}
	}
	sort.Strings(result)
	return result
}

func filterTest(modules []string) []string {
	var result []string
	for _, m := range modules {
		if strings.HasPrefix(m, "test") {
			result = append(result, m)
		}
	}
	sort.Strings(result)
	return result
}

// ---------------------------------------------------------------------------
// Infrastructure scanning
// ---------------------------------------------------------------------------

func (s *Scanner) detectInfraDirs() {
	if len(s.infraDirs) > 0 {
		return
	}
	// Auto-detect common infrastructure directories
	for _, candidate := range []string{"dev-infrastructure", "infrastructure", "infra", "terraform", "bicep"} {
		if dirExists(filepath.Join(s.repoRoot, candidate)) {
			s.infraDirs = append(s.infraDirs, candidate)
		}
	}
}

func (s *Scanner) addInfraEntries(br map[string]BlastEntry) {
	for _, infraDir := range s.infraDirs {
		fullDir := filepath.Join(s.repoRoot, infraDir)

		// Scan for template directories
		templatesDir := filepath.Join(fullDir, "templates")
		if dirExists(templatesDir) {
			entries, _ := os.ReadDir(templatesDir)
			for _, e := range entries {
				if e.IsDir() || (!strings.HasSuffix(e.Name(), ".bicep") &&
					!strings.HasSuffix(e.Name(), ".tf") &&
					!strings.HasSuffix(e.Name(), ".json")) {
					continue
				}
				path := infraDir + "/templates/" + e.Name()
				impact, scope := classifyInfraTemplate(e.Name())
				br[path] = BlastEntry{
					Impact: impact,
					Scope:  scope,
					Reason: fmt.Sprintf("Infrastructure template — %s", e.Name()),
				}
			}
		}

		// Scan for module directories
		modulesDir := filepath.Join(fullDir, "modules")
		if dirExists(modulesDir) {
			children := make(map[string]string)
			entries, _ := os.ReadDir(modulesDir)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				children[e.Name()+"/"] = "infrastructure module"
			}
			br[infraDir+"/modules/"] = BlastEntry{
				Impact:   "high",
				Scope:    "varies by module",
				Reason:   fmt.Sprintf("Reusable infrastructure modules — %d modules", len(children)),
				Children: children,
			}
		}
	}
}

func classifyInfraTemplate(name string) (impact, scope string) {
	switch {
	case strings.Contains(name, "global"):
		return "critical", "all environments, all regions, all clusters"
	case strings.Contains(name, "region"):
		return "high", "all clusters in one region"
	case strings.Contains(name, "svc"):
		return "medium", "one service cluster"
	case strings.Contains(name, "mgmt"):
		return "medium", "one management cluster"
	default:
		return "medium", "infrastructure"
	}
}

func (s *Scanner) addNonModuleServices(br map[string]BlastEntry) {
	entries, _ := os.ReadDir(s.repoRoot)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := name + "/"
		if _, exists := br[dir]; exists {
			continue
		}
		if strings.HasPrefix(name, ".") || name == "vendor" || name == "docs" ||
			name == "hack" || name == "demo" || name == "testdata" {
			continue
		}

		fullDir := filepath.Join(s.repoRoot, name)
		if dirExists(filepath.Join(fullDir, "deploy")) || fileExists(filepath.Join(fullDir, "pipeline.yaml")) {
			target := s.detectDeployTargetForDir(name)
			br[dir] = BlastEntry{
				Impact:       "low",
				Scope:        name + " service",
				Reason:       "Deployed service (non-Go module)",
				DeployTarget: target,
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Cross-component interaction derivation
// ---------------------------------------------------------------------------

func (s *Scanner) deriveInteractions() []Interaction {
	var interactions []Interaction

	// Find services (non-test, non-tooling modules) that share external deps
	extDepUsers := make(map[string][]string) // external module prefix -> workspace modules
	for modName, mod := range s.modules {
		for reqPath := range mod.externalReqs {
			parts := strings.Split(reqPath, "/")
			prefix := reqPath
			if len(parts) >= 3 {
				prefix = strings.Join(parts[:3], "/")
			}
			extDepUsers[prefix] = append(extDepUsers[prefix], modName)
		}
	}

	for dep, users := range extDepUsers {
		services := filterNonTest(users)
		services = filterNonTooling(services)
		if len(services) < 2 {
			continue
		}
		sort.Strings(services)

		nameParts := strings.Split(dep, "/")
		shortName := nameParts[len(nameParts)-1]

		interactions = append(interactions, Interaction{
			Name:        fmt.Sprintf("shared dependency: %s", shortName),
			Description: fmt.Sprintf("%s is imported by %s", shortName, strings.Join(services, ", ")),
			Implication: fmt.Sprintf("Changes to %s affect all of: %s", shortName, strings.Join(services, ", ")),
		})
	}

	// Detect modules that share a core dependency (like "internal")
	for _, core := range s.revGraph {
		if len(core) < 2 {
			continue
		}
		services := filterNonTest(core)
		services = filterNonTooling(services)
		if len(services) >= 2 {
			sort.Strings(services)
			interactions = append(interactions, Interaction{
				Name:        "shared internal types",
				Description: fmt.Sprintf("Multiple services share internal packages: %s", strings.Join(services, ", ")),
				Implication: "Type changes in shared packages affect serialization/API contracts across these services",
			})
			break // only report once
		}
	}

	sort.Slice(interactions, func(i, j int) bool { return interactions[i].Name < interactions[j].Name })

	// Deduplicate
	return slices.CompactFunc(interactions, func(a, b Interaction) bool { return a.Name == b.Name })
}

func filterNonTooling(modules []string) []string {
	var result []string
	for _, m := range modules {
		if !strings.HasPrefix(m, "tooling/") {
			result = append(result, m)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mapKeys(m map[string]bool) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
