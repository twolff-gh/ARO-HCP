package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	searchHTTPTimeout  = 60 * time.Second
	defaultMaxMatches  = 5
	minRunIDDigits     = 19
	minSuffixTokenLen  = 6
	maxSuffixTokenLen  = 8
	minSuffixLen       = 4
	maxSuffixLen       = 8
)

// searchMatch represents a single result from the CI Search API,
// classified as either a test failure or an issue tracker reference.
type searchMatch struct {
	file    string
	name    string
	context []string
	url     string
	isIssue bool
	isARO   bool
}

// searchGroup collects deduplicated matches with similar normalized context.
// Multiple runs hitting the same error are grouped under one entry.
type searchGroup struct {
	file    string
	context []string
	urls    []string
	names   []string
}

type searchHit struct {
	Name    string   `json:"name"`
	File    string   `json:"filename"`
	Context []string `json:"context"`
	URL     string   `json:"url"`
}

type searchHitGroup struct {
	Matches []searchHit `json:"matches"`
}

type ciSearchResponse struct {
	Results map[string]searchHitGroup `json:"results"`
}

// runSearch queries the OpenShift CI Search API for a pattern across runs,
// groups results by normalized error signature, separates test failures
// from issue tracker matches, and displays an ARO-HCP vs fleet-wide signal.
func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	age := fs.String("age", "168h", "max age")
	job := fs.String("job", "", "job name filter regex (overrides --env)")
	env := fs.String("env", "", "environment filter: int, stg, prod")
	max := fs.Int("max", defaultMaxMatches, "max matches per file")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: search <pattern> [--age=168h] [--env=...] [--job=...]")
	}

	jobFilter := searchJobFilter(*env, *job)
	matches, truncated, err := fetchSearchResults(fs.Arg(0), jobFilter, *age, *max)
	if err != nil {
		return err
	}

	testMatches, issueMatches := splitByType(matches)
	groups, groupOrder := deduplicateMatches(testMatches)

	aro := 0
	for _, m := range testMatches {
		if m.isARO {
			aro++
		}
	}

	searchURL := "https://search.dptools.openshift.org/?search=" + url.QueryEscape(fs.Arg(0)) +
		"&maxAge=" + url.QueryEscape(*age) + "&context=1&type=all&name=" + url.QueryEscape(jobFilter)

	return emitSearchJSON(fs.Arg(0), searchURL, groups, groupOrder, issueMatches, len(testMatches), aro, truncated)
}

// searchJobFilter resolves the job name filter from --env and --job flags.
func searchJobFilter(env, job string) string {
	if job != "" {
		return job
	}
	return defaultJobFilter(env)
}

func fetchSearchResults(pattern, jobFilter, age string, maxMatches int) ([]searchMatch, bool, error) {
	reqURL := "https://search.dptools.openshift.org/v2/search?" + url.Values{
		"search":     {pattern},
		"name":       {jobFilter},
		"maxAge":     {age},
		"maxMatches": {fmt.Sprintf("%d", maxMatches)},
		"type":       {"all"},
	}.Encode()

	client := &http.Client{Timeout: searchHTTPTimeout}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("reading search response: %w", err)
	}

	var result ciSearchResponse
	if err := json.Unmarshal(data, &result); err != nil {
		text := string(data)
		if strings.Contains(strings.ToLower(text), "maximum search length") {
			fmt.Fprintln(os.Stderr, "warning: search results truncated by CI Search API — try a more specific pattern or shorter --age")
			return nil, true, nil
		}
		fmt.Fprint(os.Stderr, text)
		return nil, false, nil
	}

	var matches []searchMatch
	for _, r := range result.Results {
		for _, m := range r.Matches {
			matches = append(matches, searchMatch{
				file:    m.File,
				name:    m.Name,
				context: m.Context,
				url:     m.URL,
				isIssue: m.File == "issue",
				isARO:   strings.Contains(m.Name, "ARO-HCP"),
			})
		}
	}
	return matches, false, nil
}

// splitByType separates search matches into test failures (junit, build-log)
// and issue tracker references (OCPBUGS, OSD, etc.).
func splitByType(matches []searchMatch) (testMatches, issueMatches []searchMatch) {
	for _, m := range matches {
		if m.isIssue {
			issueMatches = append(issueMatches, m)
		} else {
			testMatches = append(testMatches, m)
		}
	}
	return
}

// deduplicateMatches groups test matches by normalized context so the same
// error pattern across multiple runs appears as a single entry with multiple URLs.
func deduplicateMatches(matches []searchMatch) (map[string]*searchGroup, []string) {
	groups := map[string]*searchGroup{}
	var order []string
	for _, m := range matches {
		key := normalizeContext(m.file, m.context)
		g, ok := groups[key]
		if !ok {
			g = &searchGroup{file: m.file, context: m.context}
			groups[key] = g
			order = append(order, key)
		}
		g.urls = append(g.urls, m.url)
		if !slices.Contains(g.names, m.name) {
			g.names = append(g.names, m.name)
		}
	}
	return groups, order
}

// normalizeContext strips dynamic tokens (timestamps, run IDs, resource group
// suffixes) from match context so similar errors can be grouped.
func normalizeContext(file string, lines []string) string {
	joined := file + "|" + strings.Join(lines, "\n")
	return replaceDynamicTokens(joined)
}

func replaceDynamicTokens(s string) string {
	s = stripTimestamps(s)
	var b strings.Builder
	i := 0
	for i < len(s) {
		if n := matchLongNumber(s, i); n > 0 {
			b.WriteString("…")
			i += n
		} else if n, skip := matchSuffixedToken(s, i); skip {
			b.WriteString("…")
			i += n
		} else if n > 0 {
			b.WriteString(s[i : i+n])
			i += n
		} else {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// matchLongNumber checks for numeric IDs with 19+ digits (Prow run IDs).
// Returns the number of digits consumed, or 0 if under 19 digits.
func matchLongNumber(s string, i int) int {
	if !isDigit(s[i]) {
		return 0
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j-i >= minRunIDDigits {
		return j - i
	}
	return 0
}

// matchSuffixedToken checks for resource-group-style suffixes (e.g. "idms-j4g4gw").
// Only matches when the suffix contains at least one digit, which distinguishes
// random generated suffixes from real compound identifiers like "cilium-cluster".
// Returns (length, true) if the token should be replaced, (length, false) if it
// should be kept as-is, or (0, false) if not a lowercase-alnum token.
func matchSuffixedToken(s string, i int) (int, bool) {
	if !isLowerAlnum(s[i]) {
		return 0, false
	}
	j := i
	for j < len(s) && isLowerAlnum(s[j]) {
		j++
	}
	tokenLen := j - i
	if tokenLen >= minSuffixTokenLen && tokenLen <= maxSuffixTokenLen && j < len(s) && (s[j] == '-' || s[j] == '_') {
		k := j + 1
		hasDigit := false
		for k < len(s) && isLowerAlnum(s[k]) {
			if isDigit(s[k]) {
				hasDigit = true
			}
			k++
		}
		if suffixLen := k - j - 1; suffixLen >= minSuffixLen && suffixLen <= maxSuffixLen && hasDigit {
			return k - i, true
		}
	}
	return j - i, false
}

func isDigit(b byte) bool     { return b >= '0' && b <= '9' }
func isLowerAlnum(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') }

type searchResultJSON struct {
	Pattern    string             `json:"pattern"`
	SearchURL  string             `json:"search_url"`
	TotalTests int                `json:"total_tests"`
	AROCount   int                `json:"aro_count"`
	OtherCount int                `json:"other_count"`
	Truncated  bool               `json:"truncated"`
	Groups     []searchGroupJSON  `json:"groups"`
	Issues     []searchIssueJSON  `json:"issues,omitempty"`
}

type searchGroupJSON struct {
	File    string   `json:"file"`
	Names   []string `json:"names"`
	Context []string `json:"context"`
	URLs    []string `json:"urls"`
	Count   int      `json:"count"`
}

type searchIssueJSON struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func emitSearchJSON(pattern, searchURL string, groups map[string]*searchGroup, order []string, issues []searchMatch, totalTests, aro int, truncated bool) error {
	result := searchResultJSON{
		Pattern:    pattern,
		SearchURL:  searchURL,
		TotalTests: totalTests,
		AROCount:   aro,
		OtherCount: totalTests - aro,
		Truncated:  truncated,
	}
	for _, key := range order {
		g := groups[key]
		result.Groups = append(result.Groups, searchGroupJSON{
			File:    g.file,
			Names:   g.names,
			Context: g.context,
			URLs:    g.urls,
			Count:   len(g.urls),
		})
	}
	for _, m := range issues {
		result.Issues = append(result.Issues, searchIssueJSON{Name: m.name, URL: m.url})
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

