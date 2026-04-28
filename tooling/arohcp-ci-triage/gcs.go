package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	gcsBucket      = "test-platform-results"
	gcsDownload    = "https://storage.googleapis.com/" + gcsBucket + "/"
	gcsAPI         = "https://storage.googleapis.com/storage/v1/b/" + gcsBucket + "/o"
	gcsWeb         = "https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/" + gcsBucket + "/"
	gcsHTTPTimeout = 120 * time.Second
	cacheKeyLen    = 32
)

// gcs is a client for fetching CI artifacts from Google Cloud Storage.
// Artifacts are cached locally by SHA256 hash (immutable content).
type gcs struct {
	c        *http.Client
	cacheDir string
}

func newGCS() *gcs {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "arohcp-ci-triage")
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cache dir %s: %v\n", dir, err)
	}
	return &gcs{c: &http.Client{Timeout: gcsHTTPTimeout}, cacheDir: dir}
}

func (g *gcs) fetch(rawURL string) ([]byte, error) {
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(rawURL)))[:cacheKeyLen]
	cachePath := filepath.Join(g.cacheDir, cacheKey)
	if data, err := os.ReadFile(cachePath); err == nil {
		return data, nil
	}
	resp, err := g.c.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("not found: %s", rawURL)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, rawURL)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rawURL, err)
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		if gzReader, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
			if decompressed, err := io.ReadAll(gzReader); err == nil {
				data = decompressed
			} else {
				fmt.Fprintf(os.Stderr, "warning: gzip decompression failed for %s: %v\n", rawURL, err)
			}
			gzReader.Close()
		}
	}
	if len(data) > 0 {
		_ = os.WriteFile(cachePath, data, 0644)
	}
	return data, nil
}

func (g *gcs) artifact(base, path string) ([]byte, error) {
	return g.fetch(gcsDownload + base + path)
}

type gcsObject struct {
	Name string `json:"name"`
}

type gcsListResponse struct {
	Prefixes      []string    `json:"prefixes"`
	Items         []gcsObject `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
}

func (g *gcs) listDir(prefix string) (dirs []string, files []string, err error) {
	baseURL := gcsAPI + "?prefix=" + url.QueryEscape(prefix) + "&delimiter=/"
	apiURL := baseURL
	for {
		resp, err := g.c.Get(apiURL)
		if err != nil {
			return nil, nil, err
		}
		var r gcsListResponse
		decErr := json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if decErr != nil {
			return nil, nil, decErr
		}
		dirs = append(dirs, r.Prefixes...)
		for _, item := range r.Items {
			files = append(files, item.Name)
		}
		if r.NextPageToken == "" {
			break
		}
		apiURL = baseURL + "&pageToken=" + url.QueryEscape(r.NextPageToken)
	}
	return dirs, files, nil
}

// gcsBase extracts the GCS object prefix from a Prow job URL.
func gcsBase(prowURL string) string {
	const prefix = "https://prow.ci.openshift.org/view/gs/" + gcsBucket + "/"
	p, found := strings.CutPrefix(prowURL, prefix)
	if !found {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func sanitizeTest(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}

// findExtensionResultFile returns the first file matching the extension_test_result pattern.
func findExtensionResultFile(files []string) string {
	for _, f := range files {
		if strings.Contains(f, "extension_test_result_e2e_") {
			return f
		}
	}
	return ""
}
