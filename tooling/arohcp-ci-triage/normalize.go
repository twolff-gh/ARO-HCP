package main

import (
	"regexp"
	"strings"
)

const signatureKeyLen = 120

var normalizers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Structured log boilerplate
	{regexp.MustCompile(`time=\d{4}-\d{2}-\d{2}T[\d:.]+Z\s*`), ""},
	{regexp.MustCompile(`level=\w+\s+`), ""},
	{regexp.MustCompile(`msg="[^"]*"\s*`), ""},
	{regexp.MustCompile(`serviceGroup=\S+\s*`), ""},
	{regexp.MustCompile(`resourceGroup=\S+\s*`), ""},
	{regexp.MustCompile(`step=\S+\s*`), ""},
	{regexp.MustCompile(`description="[^"]*"\s*`), ""},

	// Ginkgo/Gomega boilerplate
	{regexp.MustCompile(`fail \[.*?\.go:\d+\]:\s*`), ""},
	{regexp.MustCompile(`(?i)unexpected error:\s*`), ""},
	{regexp.MustCompile(`<\*[\w.]+ \| 0x[0-9a-f]+>:\s*`), ""},

	// Go pointer addresses
	{regexp.MustCompile(`0x[0-9a-f]{8,}`), "{ADDR}"},

	// Quoted variable names
	{regexp.MustCompile(`(cluster|resourcegroup|nodepool)="[^"]+"`), "${1}={NAME}"},
	// Unquoted variable names in prose (cluster names always contain hyphens)
	{regexp.MustCompile(`(?:HCP |for )cluster [a-z0-9]+-[a-z0-9-]+`), "cluster {NAME}"},
	{regexp.MustCompile(`resource group \S+`), "resource group {NAME}"},
	{regexp.MustCompile(`NodePool \S+`), "NodePool {NAME}"},

	// Subscription IDs
	{regexp.MustCompile(`/subscriptions/[0-9a-f-]{36}`), "/subscriptions/{SUB}"},

	// Timeout precision
	{regexp.MustCompile(`timeout '[\d.]+' minutes`), "timeout N minutes"},
	{regexp.MustCompile(`Timed out after [\d.]+s`), "Timed out after Ns"},

	// API version suffixes
	{regexp.MustCompile(`CreateHCPCluster\d+`), "CreateHCPCluster"},

	// ANSI escape codes
	{regexp.MustCompile("\x1b\\[[0-9;]*m"), ""},
	{regexp.MustCompile(`\x{fffd}\[[0-9;]*m`), ""},
}

func normalizeError(msg string) string {
	for _, n := range normalizers {
		msg = n.re.ReplaceAllString(msg, n.repl)
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > signatureKeyLen {
		msg = msg[:signatureKeyLen]
	}
	return msg
}
