package main

import "testing"

func TestNormalizeError(t *testing.T) {
	tests := []struct {
		desc string
		in   string
		want string
	}{
		{
			"strips Ginkgo file:line prefix",
			`fail [github.com/Azure/ARO-HCP/test/e2e/idms_lifecycle.go:112]: Unexpected error: timeout exceeded`,
			"timeout exceeded",
		},
		{
			"strips Go error type wrappers",
			`<*fmt.wrapErrors | 0xc001676c90>: failed to create HCP cluster`,
			"failed to create HCP cluster",
		},
		{
			"normalizes cluster and resourcegroup names",
			`failed to create HCP cluster cluster="cilium-cluster" in resourcegroup="complex-cilium-kv-59b2rn"`,
			`failed to create HCP cluster cluster={NAME} in resourcegroup={NAME}`,
		},
		{
			"normalizes resource group in prose",
			`failed waiting for cluster in resource group clusternp128-jzrfpc to finish`,
			`failed waiting for cluster in resource group {NAME} to finish`,
		},
		{
			"normalizes timeout precision",
			`timeout '45.000000' minutes exceeded during CreateHCPCluster20251223FromParam`,
			`timeout N minutes exceeded during CreateHCPClusterFromParam`,
		},
		{
			"normalizes Timed out after",
			`Timed out after 2700.002s. Cluster should become ready`,
			`Timed out after Ns. Cluster should become ready`,
		},
		{
			"strips subscription IDs",
			`GET https://management.azure.com/subscriptions/aaaabbbb-cccc-dddd-eeee-ffffffffffff/resourceGroups/rg`,
			`GET https://management.azure.com/subscriptions/{SUB}/resourceGroups/rg`,
		},
		{
			"strips Go pointer addresses",
			`some error at 0xc000e81ef0 and 0xdeadbeef12`,
			`some error at {ADDR} and {ADDR}`,
		},
		{
			"truncates to signatureKeyLen",
			`fail [file.go:1]: ` + string(make([]byte, 200)),
			"", // will be 120 chars of null bytes after stripping prefix
		},
		{
			"normalizes whitespace",
			"  error \n\t with   extra   spaces  ",
			"error with extra spaces",
		},
		{
			"groups same errors with different cluster names",
			`fail [github.com/Azure/ARO-HCP/test/e2e/cluster_create_nodepool_osdisk.go:80]: Unexpected error: <*fmt.wrapErrors | 0xc0011e66c0>: failed to create HCP cluster hcp-cluster-np-128, caused by: timeout '45.000000' minutes exceeded`,
			`failed to create cluster {NAME}, caused by: timeout N minutes exceeded`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := normalizeError(tt.in)
			if tt.want != "" && got != tt.want {
				t.Errorf("normalizeError() =\n  %q\nwant\n  %q", got, tt.want)
			}
			if len(got) > signatureKeyLen {
				t.Errorf("normalizeError() length %d exceeds max %d", len(got), signatureKeyLen)
			}
		})
	}
}

func TestNormalizeError_SameSignature(t *testing.T) {
	errors := []string{
		`fail [github.com/Azure/ARO-HCP/test/e2e/cluster_create_nodepool_osdisk.go:80]: Unexpected error: <*fmt.wrapErrors | 0xc0011e66c0>: failed to create HCP cluster hcp-cluster-np-128, caused by: timeout '45.000000' minutes exceeded during CreateHCPCluster20251223FromParam for cluster hcp-cluster-np-128 in resource group clusternp128-jzrfpc`,
		`fail [github.com/Azure/ARO-HCP/test/e2e/arm64_nodepool.go:90]: Unexpected error: <*fmt.wrapErrors | 0xc001178e40>: failed to create HCP cluster arm64-vm-hcp-cluster, caused by: timeout '45.000000' minutes exceeded during CreateHCPCluster20251223FromParam for cluster arm64-vm-hcp-cluster in resource group arm64-vm-cluster-2hmkbl`,
		`fail [github.com/Azure/ARO-HCP/test/e2e/cluster_autoscaling.go:97]: Unexpected error: <*fmt.wrapErrors | 0xc001292870>: failed to create HCP cluster autoscaling-hcp-cluster, caused by: timeout '45.000000' minutes exceeded during CreateHCPClusterFromParam for cluster autoscaling-hcp-cluster in resource group autoscaling-cluster-p6txb2`,
	}

	sigs := map[string]bool{}
	for _, e := range errors {
		sigs[normalizeError(e)] = true
	}
	if len(sigs) != 1 {
		t.Errorf("expected all 3 errors to normalize to same signature, got %d signatures:", len(sigs))
		for s := range sigs {
			t.Logf("  %q", s)
		}
	}
}
