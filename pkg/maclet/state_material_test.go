package maclet

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureControllerClientMaterialUsesAgentToken(t *testing.T) {
	var requestSeen bool
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1-k3s/client-k3s-controller.crt" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "node" || password != "join-token" {
			t.Fatalf("basic auth = %q/%q/%v", username, password, ok)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read CSR: %v", err)
		}
		if _, err := x509.ParseCertificateRequest(body); err != nil {
			t.Fatalf("parse CSR: %v", err)
		}
		requestSeen = true
		response.Header().Set("Content-Type", "application/x-pem-file")
		_ = pem.Encode(response, &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, nil, "", "", true, "node", "join-token")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "state.json")
	state := &LocalState{Version: 1, Server: server.URL}
	if err := ensureControllerClientMaterial(context.Background(), client, state, statePath); err != nil {
		t.Fatal(err)
	}
	if !requestSeen {
		t.Fatal("controller certificate request was not observed")
	}
	if err := ensureControllerClientMaterial(context.Background(), client, state, statePath); err != nil {
		t.Fatalf("reuse controller material: %v", err)
	}
	if !requestSeen {
		t.Fatal("controller certificate request was not observed")
	}
	if state.ControllerCert != filepath.Join(stateDir, "client-k3s-controller.crt") || state.ControllerKey != filepath.Join(stateDir, "client-k3s-controller.key") {
		t.Fatalf("controller paths = %q/%q", state.ControllerCert, state.ControllerKey)
	}
	for _, path := range []string{state.ControllerCert, state.ControllerKey, statePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
	}
}
