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

// Package gcs fetches CI artifacts from the test-platform-results GCS bucket
// with a local disk cache keyed by content hash. Resolves Prow URLs to GCS
// paths and provides per-run artifact accessors.
package gcs

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	bucket          = "test-platform-results"
	downloadBaseURL = "https://storage.googleapis.com/" + bucket + "/"
	listBaseURL     = "https://storage.googleapis.com/storage/v1/b/" + bucket + "/o"
	httpTimeout     = 30 * time.Second
	maxArtifactSize = 10 << 20 // 10 MB; largest artifact (build-log.txt) is ~2 MB
	maxRetries      = 3        // exponential backoff on 5xx or connection errors
	cacheKeyLen     = 32       // half of SHA-256 hex; collision risk is negligible for ~10K artifacts
)

// Fetcher is the interface for fetching CI artifacts from GCS.
type Fetcher interface {
	Artifact(path string) ([]byte, error)
	ListDir(prefix string) (dirs []string, files []string, err error)
	FindByPrefix(prefix, suffix string) ([]byte, error)
}

// Client fetches CI artifacts from the test-platform-results GCS bucket
// with a local disk cache. Cache key = SHA256(URL), write-once (CI
// artifacts are immutable). Cache lives at ~/.cache/cisignal/.
type Client struct {
	c        *http.Client
	cacheDir string
}

// NewClient creates a GCS client with HTTP caching and retry.
func NewClient() *Client {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "cisignal")

	g := &Client{c: &http.Client{Timeout: httpTimeout}, cacheDir: dir}
	if err := os.MkdirAll(g.cacheDir, 0o755); err != nil {
		slog.Warn("cache dir setup failed", "dir", g.cacheDir, "error", err)
	}
	return g
}

// Artifact downloads a single file from the GCS bucket, returning cached
// data if available. Transparently decompresses gzip responses.
func (g *Client) Artifact(path string) ([]byte, error) {
	rawURL := downloadBaseURL + path

	key := fmt.Sprintf("%x", sha256.Sum256([]byte(rawURL)))[:cacheKeyLen]
	cachePath := filepath.Join(g.cacheDir, key)
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}

	data, err := g.getWithRetry(rawURL)
	if err != nil {
		return nil, fmt.Errorf("artifact %s: %w", path, err)
	}

	if bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		if decompressed, err := decompressGzip(data); err != nil {
			slog.Warn("gzip decode failed, using raw data", "path", path, "error", err)
		} else {
			data = decompressed
		}
	}

	if len(data) > 0 {
		if err := os.WriteFile(cachePath, data, 0o644); err != nil {
			slog.Warn("cache write failed", "path", cachePath, "error", err)
		}
	}
	return data, nil
}

func decompressGzip(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(io.LimitReader(gz, maxArtifactSize))
}

// FindByPrefix lists objects matching a prefix, picks the first one
// ending with suffix, and fetches it. Used for files with dynamic
// names like extension_test_result_e2e_YYYYMMDD-HHMMSS.json.
func (g *Client) FindByPrefix(prefix, suffix string) ([]byte, error) {
	_, files, err := g.ListDir(prefix)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if strings.HasSuffix(f, suffix) {
			return g.Artifact(f)
		}
	}
	return nil, fmt.Errorf("no file matching %s*%s", prefix, suffix)
}

// ListDir lists objects and subdirectories under a GCS prefix.
func (g *Client) ListDir(prefix string) (dirs []string, files []string, err error) {
	pageToken := ""
	for {
		apiURL := listBaseURL + "?prefix=" + url.QueryEscape(prefix) + "&delimiter=/"
		if pageToken != "" {
			apiURL += "&pageToken=" + url.QueryEscape(pageToken)
		}

		data, err := g.getWithRetry(apiURL)
		if err != nil {
			return nil, nil, fmt.Errorf("list %s: %w", prefix, err)
		}

		var r struct {
			Prefixes      []string   `json:"prefixes"`
			Items         []listItem `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, nil, fmt.Errorf("list decode %s: %w", prefix, err)
		}
		dirs = append(dirs, r.Prefixes...)
		for _, item := range r.Items {
			files = append(files, item.Name)
		}
		if r.NextPageToken == "" {
			break
		}
		pageToken = r.NextPageToken
	}
	return dirs, files, nil
}

type listItem struct {
	Name string `json:"name"`
}

// BaseFromProwURL extracts the GCS base path from a Prow job URL.
// Input:  "https://prow.ci.openshift.org/view/gs/test-platform-results/logs/branch-ci-.../1234"
// Output: "logs/branch-ci-.../1234/"
func BaseFromProwURL(prowURL string) string {
	const marker = "test-platform-results/"
	_, base, found := strings.Cut(prowURL, marker)
	if !found {
		return ""
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// getWithRetry fetches a URL with exponential backoff on connection
// errors and 5xx responses. Returns the response body on 200, or
// an error on non-retryable failures (4xx) or exhausted retries.
func (g *Client) getWithRetry(rawURL string) ([]byte, error) {
	var lastErr error
	for attempt := range maxRetries {
		resp, err := g.c.Get(rawURL)
		if err != nil {
			lastErr = err
			g.backoff(attempt)
			continue
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactSize))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			return data, nil
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			g.backoff(attempt)
			continue
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil, lastErr
}

func (g *Client) backoff(attempt int) {
	if attempt < maxRetries-1 {
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
}
