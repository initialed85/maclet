package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDiscoverClusterDNSNameservers(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/namespaces/kube-system/services/kube-dns" {
			t.Fatalf("request path = %q", request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(Service{Spec: corev1.ServiceSpec{
			ClusterIPs: []string{"fd00::10", "10.43.0.10", "10.43.0.10"},
		}})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	nameservers, err := discoverClusterDNSNameservers(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.43.0.10", "fd00::10"}
	if !sameStrings(nameservers, want) {
		t.Fatalf("nameservers = %#v, want %#v", nameservers, want)
	}
}

func TestManagedResolverFileLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolver", "cluster.local")
	if err := writeManagedResolverFile(path, "cluster.local", []string{"10.43.0.10"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)
	if !strings.Contains(content, resolverManagedComment) || !strings.Contains(content, "nameserver 10.43.0.10\n") {
		t.Fatalf("resolver content = %q", content)
	}
	if err := writeManagedResolverFile(path, "cluster.local", []string{"fd00::10"}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if content = string(body); strings.Contains(content, "10.43.0.10") || !strings.Contains(content, "nameserver fd00::10\n") {
		t.Fatalf("updated resolver content = %q", content)
	}
	if err := removeManagedResolverFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("resolver file stat error after removal = %v", err)
	}
}

func TestManagedResolverFileDoesNotOverwriteUnmanagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolver", "cluster.local")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte("nameserver 192.168.1.1\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedResolverFile(path, "cluster.local", []string{"10.43.0.10"}); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("write unmanaged resolver error = %v", err)
	}
	if err := removeManagedResolverFile(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(original) {
		t.Fatalf("unmanaged resolver changed to %q", body)
	}
}
