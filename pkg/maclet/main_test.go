package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDeletionTimestamp(value string) *metav1.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	stamp := metav1.NewTime(parsed)
	return &stamp
}

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

func TestPeerGatewayAddressForCIDR(t *testing.T) {
	for index, want := range map[int]string{0: "10.42.8.2", 1: "10.42.8.254", 2: "10.42.8.253"} {
		got, err := peerGatewayAddressForCIDR("10.42.8.0/24", index)
		if err != nil {
			t.Fatalf("peerGatewayAddressForCIDR(%d): %v", index, err)
		}
		if got != want {
			t.Errorf("peerGatewayAddressForCIDR(%d) = %q, want %q", index, got, want)
		}
	}
	if _, err := peerGatewayAddressForCIDR("10.42.8.0/30", 1); err == nil {
		t.Error("peerGatewayAddressForCIDR accepted a gateway beyond a /30")
	}
}

func TestDiscoverFlannelPeers(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/nodes" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(NodeList{Items: []Node{
			{
				ObjectMeta: ObjectMeta{Name: "maclet"},
			},
			{
				ObjectMeta: ObjectMeta{
					Name: "k3s-ocnus",
					Annotations: map[string]string{
						flannelBackendTypeAnnotation:   "vxlan",
						flannelBackendDataAnnotation:   `{"VNI":1,"VtepMAC":"fa:14:03:ab:e4:83"}`,
						flannelPublicIPAnnotation:      "192.168.1.111",
						flannelSubnetManagerAnnotation: "true",
					},
				},
				Spec: NodeSpec{PodCIDR: "10.42.0.0/24"},
			},
			{
				ObjectMeta: ObjectMeta{
					Name:        "ignored",
					Annotations: map[string]string{flannelBackendTypeAnnotation: "host-gw"},
				},
			},
		}})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	peers, err := discoverFlannelPeers(context.Background(), client, "maclet")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].NodeName != "k3s-ocnus" || peers[0].PodCIDR != "10.42.0.0/24" || peers[0].VtepMAC != "fa:14:03:ab:e4:83" {
		t.Fatalf("discovered peers = %#v", peers)
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

func TestDarwinNetworkCleanupIgnoresMissingInterface(t *testing.T) {
	handle := &DarwinNetworkHandle{Interface: "definitely-not-a-real-maclet-interface", Aliases: []string{"10.42.8.3"}, Routes: []DarwinRoute{{Network: "10.42.0.0", Netmask: "255.255.0.0"}}, ARPs: []DarwinARPEntry{{IP: "10.42.8.2", MAC: "00:11:22:33:44:55"}}}
	if err := handle.cleanup(); err != nil {
		t.Fatalf("cleanup missing interface: %v", err)
	}
	if len(handle.Aliases) != 0 || len(handle.Routes) != 0 || len(handle.ARPs) != 0 {
		t.Fatalf("cleanup state = %#v", handle)
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
	podCapacity := status.Capacity[corev1.ResourcePods]
	allocatablePods := status.Allocatable[corev1.ResourcePods]
	if got, allocatable := podCapacity.Value(), allocatablePods.Value(); got != defaultMaxPods || allocatable != defaultMaxPods {
		t.Errorf("pod capacity = %d/%d, want 110/110", got, allocatable)
	}
	if got := status.DaemonEndpoints.KubeletEndpoint.Port; got != defaultKubeletPort {
		t.Fatalf("kubelet endpoint port = %d, want %d", got, defaultKubeletPort)
	}
}

func TestDesiredNode(t *testing.T) {
	node := desiredNode("maclet", "192.168.137.111")
	if node.ObjectMeta.Labels["kubernetes.io/os"] != "darwin" {
		t.Errorf("OS label = %q, want darwin", node.ObjectMeta.Labels["kubernetes.io/os"])
	}
	if node.ObjectMeta.Labels["kubernetes.io/arch"] != "arm64" {
		t.Errorf("arch label = %q, want darwin", node.ObjectMeta.Labels["kubernetes.io/arch"])
	}
	if !hasManagedTaint(node.Spec.Taints) {
		t.Fatalf("Node taints = %#v, want managed NoSchedule taint", node.Spec.Taints)
	}
	if node.Spec.Taints[0].Effect != "NoSchedule" {
		t.Errorf("taint effect = %q, want NoSchedule", node.Spec.Taints[0].Effect)
	}
}

func TestSetNodeShutdownCordon(t *testing.T) {
	var cordoned bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/nodes/maclet" || request.Method != http.MethodPatch {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload struct {
			Metadata map[string]map[string]any `json:"metadata"`
			Spec     struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		cordoned = payload.Spec.Unschedulable
		if cordoned && payload.Metadata["annotations"][shutdownCordonAnnotation] != "true" {
			t.Errorf("cordon annotation = %#v", payload.Metadata["annotations"])
		}
		if !cordoned && payload.Metadata["annotations"][shutdownCordonAnnotation] != nil {
			t.Errorf("uncordon annotation = %#v", payload.Metadata["annotations"])
		}
		annotations := map[string]string{}
		if cordoned {
			annotations[shutdownCordonAnnotation] = "true"
		}
		_ = json.NewEncoder(response).Encode(Node{ObjectMeta: ObjectMeta{Name: "maclet", Annotations: annotations}, Spec: NodeSpec{Unschedulable: cordoned}})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{ObjectMeta: ObjectMeta{Name: "maclet"}}
	node, err = setNodeShutdownCordon(context.Background(), client, node, true)
	if err != nil {
		t.Fatal(err)
	}
	if !cordoned || !node.Spec.Unschedulable || node.ObjectMeta.Annotations[shutdownCordonAnnotation] != "true" {
		t.Fatalf("cordoned node = %#v", node)
	}
	node, err = setNodeShutdownCordon(context.Background(), client, node, false)
	if err != nil {
		t.Fatal(err)
	}
	if cordoned || node.Spec.Unschedulable || node.ObjectMeta.Annotations[shutdownCordonAnnotation] != "" {
		t.Fatalf("uncordoned node = %#v", node)
	}
}

func TestCleanupTerminatingPods(t *testing.T) {
	deleted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if request.URL.Path != "/api/v1/namespaces/default/pods" {
				t.Fatalf("list request path = %s", request.URL.Path)
			}
			if got := request.URL.Query().Get("fieldSelector"); got != "spec.nodeName=maclet" {
				t.Errorf("fieldSelector = %q", got)
			}
			if got := request.URL.Query().Get("labelSelector"); got != nativeWorkloadLabelKey+"="+nativeWorkloadLabelValue {
				t.Errorf("labelSelector = %q", got)
			}
			_ = json.NewEncoder(response).Encode(PodList{Items: []Pod{
				{
					ObjectMeta: ObjectMeta{Namespace: "default", Name: "stale", UID: "uid-stale", Labels: map[string]string{nativeWorkloadLabelKey: nativeWorkloadLabelValue}, DeletionTimestamp: testDeletionTimestamp("2026-08-30T17:00:00Z")},
					Spec:       PodSpec{NodeName: "maclet"},
				},
				{
					ObjectMeta: ObjectMeta{Namespace: "default", Name: "fresh", UID: "uid-fresh", Labels: map[string]string{nativeWorkloadLabelKey: nativeWorkloadLabelValue}},
					Spec:       PodSpec{NodeName: "maclet"},
				},
				{
					ObjectMeta: ObjectMeta{Namespace: "default", Name: "other-node", UID: "uid-other", Labels: map[string]string{nativeWorkloadLabelKey: nativeWorkloadLabelValue}, DeletionTimestamp: testDeletionTimestamp("2026-08-30T17:00:00Z")},
					Spec:       PodSpec{NodeName: "other"},
				},
				{
					ObjectMeta: ObjectMeta{Namespace: "default", Name: "unmanaged", UID: "uid-unmanaged", Labels: map[string]string{"example.test/owner": "someone-else"}, DeletionTimestamp: testDeletionTimestamp("2026-08-30T17:00:00Z")},
					Spec:       PodSpec{NodeName: "maclet"},
				},
			}})
		case http.MethodDelete:
			if request.URL.Path != "/api/v1/namespaces/default/pods/stale" || request.URL.Query().Get("gracePeriodSeconds") != "0" {
				t.Fatalf("delete request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			deleted = true
			response.WriteHeader(http.StatusOK)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := cleanupTerminatingPods(context.Background(), client, "maclet", "default", time.Minute, time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || !deleted {
		t.Fatalf("cleanup result = removed %d, deleted %v", removed, deleted)
	}
}

func TestCleanupTerminatingPodsClusterWide(t *testing.T) {
	deleted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if request.URL.Path != "/api/v1/pods" {
				t.Fatalf("list request path = %s", request.URL.Path)
			}
			if request.URL.Query().Get("fieldSelector") != "spec.nodeName=maclet" || request.URL.Query().Get("labelSelector") != nativeWorkloadLabelKey+"="+nativeWorkloadLabelValue {
				t.Fatalf("list query = %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(response).Encode(PodList{Items: []Pod{{
				ObjectMeta: ObjectMeta{Namespace: "other", Name: "stale", UID: "uid-stale", Labels: map[string]string{nativeWorkloadLabelKey: nativeWorkloadLabelValue}, DeletionTimestamp: testDeletionTimestamp("2026-08-30T17:00:00Z")},
				Spec:       PodSpec{NodeName: "maclet"},
			}}})
		case http.MethodDelete:
			if request.URL.Path != "/api/v1/namespaces/other/pods/stale" || request.URL.Query().Get("gracePeriodSeconds") != "0" {
				t.Fatalf("delete request = %s?%s", request.URL.Path, request.URL.RawQuery)
			}
			deleted = true
			response.WriteHeader(http.StatusOK)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := cleanupTerminatingPodsClusterWide(context.Background(), client, "maclet", time.Minute, time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || !deleted {
		t.Fatalf("cleanup result = removed %d, deleted %v", removed, deleted)
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
			ObjectMeta: ObjectMeta{Namespace: "default", Name: "hello", UID: "uid-1"},
			Spec:       PodSpec{NodeName: "maclet", Containers: []ContainerSpec{{Name: "app", Image: "example/hello:latest"}}},
			Status:     PodStatus{Phase: "Pending"},
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
