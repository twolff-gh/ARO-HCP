package main

import (
	"regexp"
	"strings"
)

// TestLink maps a test name to its resource group and Kusto links.
type TestLink struct {
	TestName      string `json:"test_name"`
	ResourceGroup string `json:"resource_group"`
	KustoCluster  string `json:"kusto_cluster,omitempty"`
}

var (
	linkRGRe    = regexp.MustCompile(`--resource-group[= ]([a-zA-Z0-9_-]+)`)
	linkKustoRe = regexp.MustCompile(`(hcp-\S+\.(?:eastus2|uksouth|westeurope|centralindia|australiaeast|canadacentral|switzerlandnorth|brazilsouth)\S*)`)
)

// extractTestLinks parses custom-link-tools HTML to map resource groups to Kusto clusters.
func extractTestLinks(html string) []TestLink {
	sections := strings.Split(html, "<h")
	var links []TestLink
	seen := map[string]bool{}

	for _, sec := range sections {
		rgs := linkRGRe.FindAllStringSubmatch(sec, -1)
		if len(rgs) == 0 {
			continue
		}

		var kusto string
		if m := linkKustoRe.FindStringSubmatch(sec); len(m) > 1 {
			kusto = m[1]
		}

		for _, rg := range rgs {
			rgName := rg[1]
			if seen[rgName] {
				continue
			}
			seen[rgName] = true
			links = append(links, TestLink{
				ResourceGroup: rgName,
				KustoCluster:  kusto,
			})
		}
	}
	return links
}
