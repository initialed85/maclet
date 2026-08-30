package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTokenPassword(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		password string
		hash     string
	}{
		{
			name:     "full K10 token",
			token:    "K10" + strings.Repeat("a", 64) + "::server:secret",
			password: "secret",
			hash:     strings.Repeat("a", 64),
		},
		{name: "basic credential", token: "node:secret", password: "secret"},
		{name: "bare password", token: "secret", password: "secret"},
		{name: "K10 token without CA hash", token: "K10::node:secret", password: "secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			password, hash, err := tokenPassword(test.token)
			if err != nil {
				t.Fatal(err)
			}
			if password != test.password || hash != test.hash {
				t.Fatalf("tokenPassword() = (%q, %q), want (%q, %q)", password, hash, test.password, test.hash)
			}
		})
	}
}

func TestTokenPasswordRejectsEmptyPassword(t *testing.T) {
	for _, token := range []string{"", "node:", "K10::node:"} {
		if _, _, err := tokenPassword(token); err == nil {
			t.Errorf("tokenPassword(%q) unexpectedly succeeded", token)
		}
	}
}

func TestGatewayAddressForCIDR(t *testing.T) {
	for cidr, want := range map[string]string{
		"10.42.8.0/24":     "10.42.8.2",
		"192.168.100.0/16": "192.168.0.2",
	} {
		got, err := gatewayAddressForCIDR(cidr)
		if err != nil {
			t.Fatalf("gatewayAddressForCIDR(%q): %v", cidr, err)
		}
		if got != want {
			t.Errorf("gatewayAddressForCIDR(%q) = %q, want %q", cidr, got, want)
		}
	}
	if _, err := gatewayAddressForCIDR("10.42.8.0/31"); err == nil {
		t.Error("gatewayAddressForCIDR() accepted a CIDR without a second usable address")
	}
}

func TestRouteSpec(t *testing.T) {
	route, err := routeSpec("10.42.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if route.Network != "10.42.0.0" || route.Netmask != "255.255.0.0" {
		t.Fatalf("route = %#v", route)
	}
}

func TestWorkloadIPForOffset(t *testing.T) {
	for _, test := range []struct {
		cidr   string
		offset uint32
		want   string
	}{
		{cidr: "10.42.8.0/24", offset: 3, want: "10.42.8.3"},
		{cidr: "10.42.8.0/24", offset: 254, want: "10.42.8.254"},
		{cidr: "192.168.100.0/16", offset: 3, want: "192.168.0.3"},
	} {
		got, err := workloadIPForOffset(test.cidr, test.offset)
		if err != nil {
			t.Fatalf("workloadIPForOffset(%q, %d): %v", test.cidr, test.offset, err)
		}
		if got != test.want {
			t.Errorf("workloadIPForOffset(%q, %d) = %q, want %q", test.cidr, test.offset, got, test.want)
		}
	}
	for _, test := range []struct {
		cidr   string
		offset uint32
	}{
		{cidr: "10.42.8.0/24", offset: 0},
		{cidr: "10.42.8.0/24", offset: 1},
		{cidr: "10.42.8.0/24", offset: 2},
		{cidr: "10.42.8.0/24", offset: 255},
		{cidr: "10.42.8.0/31", offset: 3},
		{cidr: "2001:db8::/64", offset: 3},
	} {
		if _, err := workloadIPForOffset(test.cidr, test.offset); err == nil {
			t.Errorf("workloadIPForOffset(%q, %d) unexpectedly succeeded", test.cidr, test.offset)
		}
	}
}

func TestFirstAvailableWorkloadIP(t *testing.T) {
	used := map[string]bool{"10.42.8.3": true, "10.42.8.4": true}
	got, err := firstAvailableWorkloadIP("10.42.8.0/24", used)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.42.8.5" {
		t.Fatalf("firstAvailableWorkloadIP() = %q, want 10.42.8.5", got)
	}
	used[got] = true
	shortCIDRUsed := map[string]bool{}
	got, err = firstAvailableWorkloadIP("10.42.8.0/29", shortCIDRUsed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.42.8.3" {
		t.Fatalf("firstAvailableWorkloadIP(/29) = %q, want 10.42.8.3", got)
	}
	for _, ip := range []string{"10.42.8.3", "10.42.8.4", "10.42.8.5", "10.42.8.6"} {
		shortCIDRUsed[ip] = true
	}
	if _, err := firstAvailableWorkloadIP("10.42.8.0/29", shortCIDRUsed); err == nil {
		t.Fatal("firstAvailableWorkloadIP() found an address in an exhausted CIDR")
	}
}

func TestBridgeAddressForCIDR(t *testing.T) {
	tests := map[string]string{
		"10.42.4.0/24":     "10.42.4.1/24",
		"192.168.100.0/16": "192.168.0.1/16",
	}
	for cidr, want := range tests {
		got, err := bridgeAddressForCIDR(cidr)
		if err != nil {
			t.Fatalf("bridgeAddressForCIDR(%q): %v", cidr, err)
		}
		if got != want {
			t.Errorf("bridgeAddressForCIDR(%q) = %q, want %q", cidr, got, want)
		}
	}
	if _, err := bridgeAddressForCIDR("fd00::/64"); err == nil {
		t.Error("bridgeAddressForCIDR() accepted an IPv6 CIDR")
	}
}

func TestFlannelAnnotations(t *testing.T) {
	annotations, err := flannelAnnotations("192.168.137.111", "5e:e9:1e:e7:7b:65")
	if err != nil {
		t.Fatal(err)
	}
	if annotations[flannelBackendTypeAnnotation] != "vxlan" {
		t.Errorf("backend type = %q, want vxlan", annotations[flannelBackendTypeAnnotation])
	}
	if annotations[flannelPublicIPAnnotation] != "192.168.137.111" {
		t.Errorf("public IP = %q", annotations[flannelPublicIPAnnotation])
	}
	if annotations[flannelSubnetManagerAnnotation] != "true" {
		t.Errorf("subnet manager = %q", annotations[flannelSubnetManagerAnnotation])
	}
	if got, want := annotations[flannelBackendDataAnnotation], `{"VNI":1,"VtepMAC":"5e:e9:1e:e7:7b:65"}`; got != want {
		t.Errorf("backend data = %q, want %q", got, want)
	}
}

func TestNodeStatusIncludesExternalIP(t *testing.T) {
	status := nodeStatus("maclet", "192.168.137.111", "192.168.137.111", time.Unix(0, 0))
	if len(status.Addresses) != 3 {
		t.Fatalf("addresses = %#v, want InternalIP, ExternalIP, Hostname", status.Addresses)
	}
	if status.Addresses[1].Type != "ExternalIP" || status.Addresses[1].Address != "192.168.137.111" {
		t.Errorf("external address = %#v", status.Addresses[1])
	}
	if status.Capacity["pods"] != "110" || status.Allocatable["pods"] != "110" {
		t.Errorf("pod capacity = %q/%q, want 110/110", status.Capacity["pods"], status.Allocatable["pods"])
	}
}

func TestDesiredNode(t *testing.T) {
	node := desiredNode("maclet", "192.168.137.111")
	if node.Metadata.Labels["kubernetes.io/os"] != "darwin" {
		t.Errorf("OS label = %q, want darwin", node.Metadata.Labels["kubernetes.io/os"])
	}
	if node.Metadata.Labels["kubernetes.io/arch"] != "arm64" {
		t.Errorf("arch label = %q, want arm64", node.Metadata.Labels["kubernetes.io/arch"])
	}
	if !hasManagedTaint(node.Spec.Taints) {
		t.Fatalf("Node taints = %#v, want managed NoSchedule taint", node.Spec.Taints)
	}
	if node.Spec.Taints[0].Effect != "NoSchedule" {
		t.Errorf("taint effect = %q, want NoSchedule", node.Spec.Taints[0].Effect)
	}
}

func TestWorkloadSnapshot(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/pods" {
			http.NotFound(response, request)
			return
		}
		if got := request.URL.Query().Get("fieldSelector"); got != "spec.nodeName=maclet" {
			t.Errorf("fieldSelector = %q", got)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(PodList{Items: []Pod{{
			Metadata: ObjectMeta{Namespace: "default", Name: "hello", UID: "uid-1"},
			Spec:     PodSpec{NodeName: "maclet", Containers: []ContainerSpec{{Name: "app", Image: "example/hello:latest"}}},
			Status:   PodStatus{Phase: "Pending"},
		}}})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := workloadSnapshot(context.Background(), client, "maclet")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Node != "maclet" || len(snapshot.Workloads) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if got := snapshot.Workloads[0].Containers[0]; got != "app=example/hello:latest" {
		t.Errorf("container = %q", got)
	}
}
