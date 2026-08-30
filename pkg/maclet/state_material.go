package maclet

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func certificateNeedsRefresh(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return true
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	return err != nil || time.Now().Add(time.Hour).After(certificate.NotAfter)
}

func validPEMCertificate(body []byte) bool {
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	_, err := x509.ParseCertificate(block.Bytes)
	return err == nil
}

func writeLocalState(path string, state *LocalState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(body, '\n'), 0600)
}

func ensureKubeletServerMaterial(ctx context.Context, client *APIClient, state *LocalState, statePath, nodeName, nodeIP string) error {
	stateDir := filepath.Dir(statePath)
	if state.ClientCA == "" {
		state.ClientCA = filepath.Join(stateDir, "client-ca.crt")
	}
	if state.ServingCert == "" {
		state.ServingCert = filepath.Join(stateDir, "serving-kubelet.crt")
	}
	if state.ServingKey == "" {
		state.ServingKey = filepath.Join(stateDir, "serving-kubelet.key")
	}
	clientCA, err := os.ReadFile(state.ClientCA)
	if err != nil || !validPEMCertificate(clientCA) {
		clientCA, err = client.Get(ctx, "/v1-k3s/client-ca.crt")
		if err != nil {
			return fmt.Errorf("retrieve kubelet client CA: %w", err)
		}
		if !validPEMCertificate(clientCA) {
			return errors.New("k3s returned an invalid kubelet client CA")
		}
		if err := writePrivateFile(state.ClientCA, clientCA, 0600); err != nil {
			return err
		}
	}
	if certificateNeedsRefresh(state.ServingCert) || func() bool { _, err := os.Stat(state.ServingKey); return err != nil }() {
		csrDER, keyPEM, err := generateClientCSR(nodeName)
		if err != nil {
			return fmt.Errorf("generate kubelet serving certificate key: %w", err)
		}
		passwordBody, err := os.ReadFile(state.PasswordFile)
		if err != nil {
			return fmt.Errorf("read node password: %w", err)
		}
		headers := map[string]string{
			"k3s-Node-Name":     nodeName,
			"k3s-Node-Password": strings.TrimSpace(string(passwordBody)),
		}
		if nodeIP != "" {
			headers["k3s-Node-IP"] = nodeIP
		}
		certPEM, err := client.Do(ctx, http.MethodPost, "/v1-k3s/serving-kubelet.crt", csrDER, "application/pkcs10", headers)
		if err != nil {
			return fmt.Errorf("request kubelet serving certificate: %w", err)
		}
		if !validPEMCertificate(certPEM) {
			return errors.New("k3s returned an invalid kubelet serving certificate")
		}
		if err := writePrivateFile(state.ServingKey, keyPEM, 0600); err != nil {
			return err
		}
		if err := writePrivateFile(state.ServingCert, certPEM, 0600); err != nil {
			return err
		}
	}
	return writeLocalState(statePath, state)
}
