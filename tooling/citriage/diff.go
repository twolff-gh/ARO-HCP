package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	githubHTTPTimeout  = 30 * time.Second
	diffRunsFetchLimit = 50 // how many adjacent runs to scan for version transition
)

// runDiff compares two deploy versions or auto-detects the version
// transition for a specific run. Groups commits by impact level and
// files by directory.
func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	run := fs.String("run", "", "auto-detect versions from a run ID")
	fs.Parse(args)

	if *run != "" {
		return diffFromRun(*run)
	}

	pos := fs.Args()
	if len(pos) != 2 {
		return fmt.Errorf("usage: diff <commit1> <commit2>  or  diff --run=<run-id>")
	}

	return emitDiff(pos[0], pos[1])
}

// diffFromRun finds the EV2 version transition for a run by scanning
// adjacent runs for a different deploy version.
func diffFromRun(runID string) error {
	sippyClient := newSippy()
	sum, err := sippyClient.runSummary(runID)
	if err != nil {
		return err
	}

	runs, err := sippyClient.listRuns(sum.Release, diffRunsFetchLimit, sum.Name)
	if err != nil {
		return err
	}

	var thisEV2, prevEV2 string
	found := false
	for _, r := range runs {
		ev2 := ev2Hash(r)
		if fmt.Sprintf("%d", r.ID) == runID {
			thisEV2 = ev2
			found = true
			continue
		}
		if found && ev2 != "" && ev2 != thisEV2 {
			prevEV2 = ev2
			break
		}
	}
	if thisEV2 == "" {
		return fmt.Errorf("run has no EV2 annotation (may be cron-triggered)")
	}
	if prevEV2 == "" {
		return fmt.Errorf("no previous run with different EV2 commit found")
	}

	return emitDiff(prevEV2, thisEV2)
}

type commitAuthor struct {
	Name string `json:"name"`
}

type commitInfo struct {
	Message string       `json:"message"`
	Author  commitAuthor `json:"author"`
}

type compareCommit struct {
	SHA    string     `json:"sha"`
	Commit commitInfo `json:"commit"`
}

type compareFile struct {
	Name   string `json:"filename"`
	Status string `json:"status"`
	Add    int    `json:"additions"`
	Del    int    `json:"deletions"`
}

type compareResponse struct {
	AheadBy int             `json:"ahead_by"`
	Commits []compareCommit `json:"commits"`
	Files   []compareFile   `json:"files"`
}

type diffJSON struct {
	Base    string            `json:"base"`
	Head    string            `json:"head"`
	AheadBy int               `json:"ahead_by"`
	Commits []commitJSON      `json:"commits"`
	Files   []compareFile     `json:"files"`
}

type commitJSON struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
}

// githubCompareRaw calls the GitHub compare API to get commits and files between two refs.
func githubCompareRaw(base, head string) (*compareResponse, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/Azure/ARO-HCP/compare/%s...%s", base, head)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building GitHub request: %w", err)
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: githubHTTPTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub %d: %s", resp.StatusCode, body)
	}
	var r compareResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding GitHub compare response: %w", err)
	}
	return &r, nil
}

// emitDiff fetches the GitHub comparison and writes the result as JSON to stdout.
func emitDiff(base, head string) error {
	r, err := githubCompareRaw(base, head)
	if err != nil {
		return err
	}

	d := diffJSON{Base: base, Head: head, AheadBy: r.AheadBy, Files: r.Files}
	for _, c := range r.Commits {
		d.Commits = append(d.Commits, commitJSON{
			SHA:     c.SHA,
			Message: c.Commit.Message,
			Author:  c.Commit.Author.Name,
		})
	}
	return json.NewEncoder(os.Stdout).Encode(d)
}
