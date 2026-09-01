package maclet

import "testing"

func TestDecodeK3sAgentConfig(t *testing.T) {
	clusterCIDR, serviceCIDR, clusterDNS, err := decodeK3sAgentConfig([]byte(`{
		"ClusterIPRange": {"IP": "10.42.0.0", "Mask": "//8AAA=="},
		"ServiceIPRanges": [{"IP": "10.43.0.0", "Mask": "//8AAA=="}],
		"ClusterDNSs": ["10.43.0.10", "10.43.0.11"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if clusterCIDR != "10.42.0.0/16" {
		t.Fatalf("cluster CIDR = %q", clusterCIDR)
	}
	if serviceCIDR != "10.43.0.0/16" {
		t.Fatalf("service CIDR = %q", serviceCIDR)
	}
	if got, want := len(clusterDNS), 2; got != want || clusterDNS[0] != "10.43.0.10" || clusterDNS[1] != "10.43.0.11" {
		t.Fatalf("cluster DNS = %#v", clusterDNS)
	}
}

func TestDecodeK3sAgentConfigPrefersPluralCIDRs(t *testing.T) {
	clusterCIDR, serviceCIDR, _, err := decodeK3sAgentConfig([]byte(`{
		"ClusterIPRange": {"IP": "10.42.0.0", "Mask": "//8AAA=="},
		"ClusterIPRanges": [{"IP": "10.42.8.0", "Mask": "////AA=="}],
		"ServiceIPRange": {"IP": "10.43.0.0", "Mask": "//8AAA=="}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if clusterCIDR != "10.42.8.0/24" || serviceCIDR != "10.43.0.0/16" {
		t.Fatalf("CIDRs = %q/%q", clusterCIDR, serviceCIDR)
	}
}

func TestDecodeK3sAgentConfigRejectsConfiguredIPv6OnlyCIDR(t *testing.T) {
	_, _, _, err := decodeK3sAgentConfig([]byte(`{
		"ClusterIPRanges": [{"IP": "fd00::", "Mask": "//////////8="}]
	}`))
	if err == nil || err.Error() != "K3s cluster configuration contains no IPv4 CIDR" {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeK3sAgentConfigRejectsInvalidClusterDNS(t *testing.T) {
	_, _, _, err := decodeK3sAgentConfig([]byte(`{
		"ClusterDNSs": ["not-an-ip"]
	}`))
	if err == nil {
		t.Fatal("expected invalid ClusterDNS error")
	}
}

func TestDecodeK3sAgentConfigFallsBackToSingleClusterDNS(t *testing.T) {
	_, _, clusterDNS, err := decodeK3sAgentConfig([]byte(`{
		"ClusterDNS": "10.43.0.10"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(clusterDNS) != 1 || clusterDNS[0] != "10.43.0.10" {
		t.Fatalf("cluster DNS = %#v", clusterDNS)
	}
}
