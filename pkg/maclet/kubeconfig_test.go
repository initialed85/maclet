package maclet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPeerAPIClientReadsYAMLWithoutKubectl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	contents := `apiVersion: v1
kind: Config
current-context: home
clusters:
- name: home
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: home
  context:
    cluster: home
    user: admin
users:
- name: admin
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	client, available, err := loadPeerAPIClient("", path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !available || client == nil {
		t.Fatalf("loadPeerAPIClient() = client %v, available %v; want a client", client != nil, available)
	}
}
