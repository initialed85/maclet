package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLeaveClusterResources(t *testing.T) {
	var requests []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/nodes/maclet":
			_ = json.NewEncoder(response).Encode(Node{ObjectMeta: ObjectMeta{
				Name: "maclet",
				Annotations: map[string]string{
					flannelBackendTypeAnnotation:   "vxlan",
					flannelBackendDataAnnotation:   `{"VNI":1,"VtepMAC":"aa:bb:cc:dd:ee:ff"}`,
					flannelPublicIPAnnotation:      "192.168.137.111",
					flannelSubnetManagerAnnotation: "true",
				},
			}})
		case request.Method == http.MethodPatch && request.URL.Path == "/api/v1/nodes/maclet":
			_ = json.NewEncoder(response).Encode(Node{ObjectMeta: ObjectMeta{Name: "maclet"}})
		case request.Method == http.MethodDelete:
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
	if err := leaveClusterResources(context.Background(), client, "maclet"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/v1/nodes/maclet",
		"PATCH /api/v1/nodes/maclet",
		"DELETE /apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/maclet",
		"DELETE /api/v1/namespaces/kube-system/secrets/maclet.node-password.k3s",
		"DELETE /api/v1/nodes/maclet",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Errorf("request %d = %q, want %q", index, requests[index], want[index])
		}
	}
}

func TestLeaveClusterResourcesIsIdempotent(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := leaveClusterResources(context.Background(), client, "missing"); err != nil {
		t.Fatalf("leave missing resources: %v", err)
	}
}

func TestRemoveLocalStateFiles(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{
		"state.json",
		"workloads.json",
		"server-ca.crt",
		"client-kubelet.crt",
		"client-kubelet.key",
		"client-k3s-controller.crt",
		"client-k3s-controller.key",
		"client-ca.crt",
		"serving-kubelet.crt",
		"serving-kubelet.key",
		"node-password",
	} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("state"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "preserve-me"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeLocalStateFiles(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state.json still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "preserve-me")); err != nil {
		t.Fatalf("preserved file: %v", err)
	}
	if err := os.Remove(filepath.Join(stateDir, "preserve-me")); err != nil {
		t.Fatal(err)
	}
	if err := removeLocalStateFiles(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("state directory still exists: %v", err)
	}
}

func TestLeaveResourceNotFound(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := leaveResource(context.Background(), client, "resource", "/api/v1/resource"); err != nil {
		t.Fatalf("leave resource: %v", err)
	}
}
