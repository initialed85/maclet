package maclet

import (
	"context"
	"crypto"
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

func TestEnsureControllerClientMaterialUsesReturnedPrivateKey(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1-k3s/client-k3s-controller.crt" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Fatalf("read CSR: %v", err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
		if err != nil {
			t.Fatalf("marshal server key: %v", err)
		}
		response.Header().Set("Content-Type", "application/x-pem-file")
		_ = pem.Encode(response, &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		_ = pem.Encode(response, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
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

	keyBody, err := os.ReadFile(state.ControllerKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyBody)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("stored key PEM type = %v", keyBlock)
	}
	storedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse stored key: %v", err)
	}
	certBody, err := os.ReadFile(state.ControllerCert)
	if err != nil {
		t.Fatal(err)
	}
	certBlock, _ := pem.Decode(certBody)
	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse stored cert: %v", err)
	}
	storedSigner, ok := storedKey.(crypto.Signer)
	if !ok {
		t.Fatalf("stored key does not expose a public key: %T", storedKey)
	}
	storedPublic, err := x509.MarshalPKIXPublicKey(storedSigner.Public())
	if err != nil {
		t.Fatalf("marshal stored public key: %v", err)
	}
	certificatePublic, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		t.Fatalf("marshal certificate public key: %v", err)
	}
	if string(storedPublic) != string(certificatePublic) {
		t.Fatal("stored returned private key does not match certificate")
	}
}
