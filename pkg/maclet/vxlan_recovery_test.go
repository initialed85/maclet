package maclet

import "testing"

func TestMatchingVXLANInvocation(t *testing.T) {
	command := "/opt/homebrew/bin/darwin-vxlan --vni 1 --local 192.168.137.111 --remote 192.168.1.111 --port 8472 --mtu 1450 --bridge-ipv4 10.42.5.1/24 --peer 10.42.0.0/24=192.168.1.111"
	bridge, ok := matchingVXLANInvocation(command, "/opt/homebrew/bin/darwin-vxlan", "192.168.137.111", "192.168.1.111", 8472)
	if !ok || bridge != "10.42.5.1/24" {
		t.Fatalf("matchingVXLANInvocation() = %q, %v", bridge, ok)
	}
}

func TestMatchingVXLANInvocationAcceptsSudoWrapperAndDifferentBridgeCIDR(t *testing.T) {
	command := "sudo -n /Users/edward/bin/darwin-vxlan --vni 1 --local 192.168.137.111 --remote 192.168.1.111 --port 8472 --mtu 1450 --bridge-ipv4 10.42.4.1/24"
	bridge, ok := matchingVXLANInvocation(command, "/Users/edward/bin/darwin-vxlan", "192.168.137.111", "192.168.1.111", 8472)
	if !ok || bridge != "10.42.4.1/24" {
		t.Fatalf("matchingVXLANInvocation() = %q, %v", bridge, ok)
	}
}

func TestMatchingVXLANInvocationRejectsDifferentTransport(t *testing.T) {
	command := "/opt/homebrew/bin/darwin-vxlan --vni 1 --local 192.168.137.111 --remote 192.168.1.112 --port 8472 --bridge-ipv4 10.42.5.1/24"
	if bridge, ok := matchingVXLANInvocation(command, "/opt/homebrew/bin/darwin-vxlan", "192.168.137.111", "192.168.1.111", 8472); ok || bridge != "" {
		t.Fatalf("matchingVXLANInvocation() = %q, %v for different remote", bridge, ok)
	}
}
