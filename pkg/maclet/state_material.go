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

// splitCertificateAndKeyPEM separates the certificate chain from an optional
// private key in a K3s certificate response. Current servers sign the CSR and
// return only certificates; older servers may return a certificate and the
// server-generated private key together.
func splitCertificateAndKeyPEM(body []byte) (certPEM, keyPEM []byte) {
	for {
		block, rest := pem.Decode(body)
		if block == nil {
			break
		}
		body = rest
		encoded := pem.EncodeToMemory(block)
		if strings.Contains(block.Type, "PRIVATE KEY") {
			keyPEM = append(keyPEM, encoded...)
		} else {
			certPEM = append(certPEM, encoded...)
		}
	}
	return certPEM, keyPEM
}

func writeLocalState(path string, state *LocalState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(body, '\n'), 0600)
}

func ensureControllerClientMaterial(ctx context.Context, client *APIClient, state *LocalState, statePath string) error {
	stateDir := filepath.Dir(statePath)
	if state.ControllerCert == "" {
		state.ControllerCert = filepath.Join(stateDir, "client-k3s-controller.crt")
	}
	if state.ControllerKey == "" {
		state.ControllerKey = filepath.Join(stateDir, "client-k3s-controller.key")
	}
	if validPEMCertificateFile(state.ControllerCert) {
		if _, err := os.Stat(state.ControllerKey); err == nil {
			return writeLocalState(statePath, state)
		}
	}
	csrDER, keyPEM, err := generateClientCSR("")
	if err != nil {
		return fmt.Errorf("generate k3s controller certificate key: %w", err)
	}
	responsePEM, err := client.Do(ctx, http.MethodPost, "/v1-k3s/client-k3s-controller.crt", csrDER, "application/pkcs10", nil)
	if err != nil {
		return fmt.Errorf("request k3s controller certificate: %w", err)
	}
	certPEM, returnedKeyPEM := splitCertificateAndKeyPEM(responsePEM)
	if !validPEMCertificate(certPEM) {
		return errors.New("k3s returned an invalid controller client certificate")
	}
	if len(returnedKeyPEM) > 0 {
		keyPEM = returnedKeyPEM
	}
	if err := writePrivateFile(state.ControllerKey, keyPEM, 0600); err != nil {
		return err
	}
	if err := writePrivateFile(state.ControllerCert, certPEM, 0600); err != nil {
		return err
	}
	return writeLocalState(statePath, state)
}

func validPEMCertificateFile(path string) bool {
	body, err := os.ReadFile(path)
	return err == nil && validPEMCertificate(body)
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
