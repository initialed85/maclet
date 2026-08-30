package maclet

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func bootstrap(ctx context.Context, cfg JoinConfig) (*LocalState, *APIClient, error) {
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := chownToInvokingUser(cfg.StateDir); err != nil {
		return nil, nil, err
	}
	statePath := filepath.Join(cfg.StateDir, "state.json")
	if body, err := os.ReadFile(statePath); err == nil {
		var state LocalState
		if err := json.Unmarshal(body, &state); err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", statePath, err)
		}
		if cfg.Server != "" && cfg.Server != state.Server {
			return nil, nil, fmt.Errorf("state belongs to %s, not %s", state.Server, cfg.Server)
		}
		if cfg.NodeName != "" && cfg.NodeName != state.NodeName {
			return nil, nil, fmt.Errorf("state belongs to node %s, not %s", state.NodeName, cfg.NodeName)
		}
		client, err := newAPIClient(state.Server, mustReadFile(state.CAFile), state.ClientCert, state.ClientKey, cfg.InsecureSkipTLSVerify, "", "")
		if err != nil {
			return nil, nil, err
		}
		if err := ensureKubeletServerMaterial(ctx, client, &state, statePath, state.NodeName, state.NodeIP); err != nil {
			return nil, nil, err
		}
		return &state, client, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read %s: %w", statePath, err)
	}

	if cfg.Server == "" {
		return nil, nil, errors.New("--server is required for first join")
	}
	if cfg.NodeName == "" {
		cfg.NodeName = defaultNodeName
	}
	token, err := readToken(cfg.Token, cfg.TokenFile)
	if err != nil {
		return nil, nil, err
	}
	password, expectedHash, err := tokenPassword(token)
	if err != nil {
		return nil, nil, err
	}
	caPEM, err := fetchCA(ctx, cfg.Server, expectedHash)
	if err != nil {
		return nil, nil, err
	}
	bootstrapClient, err := newAPIClient(cfg.Server, caPEM, "", "", cfg.InsecureSkipTLSVerify, "node", password)
	if err != nil {
		return nil, nil, err
	}
	if _, err := bootstrapClient.Get(ctx, "/v1-k3s/readyz"); err != nil {
		return nil, nil, fmt.Errorf("authenticate with k3s agent token: %w", err)
	}
	if _, err := bootstrapClient.Get(ctx, "/v1-k3s/config"); err != nil {
		return nil, nil, fmt.Errorf("retrieve k3s agent configuration: %w", err)
	}

	if cfg.NodeIP == "" {
		cfg.NodeIP = detectNodeIP(cfg.Server)
	}
	if cfg.ExternalIP == "" {
		cfg.ExternalIP = cfg.VXLANLocal
		if cfg.ExternalIP == "" {
			cfg.ExternalIP = cfg.NodeIP
		}
	}
	nodePassword, err := randomPassword()
	if err != nil {
		return nil, nil, err
	}
	csrPEM, keyPEM, err := generateClientCSR(cfg.NodeName)
	if err != nil {
		return nil, nil, err
	}
	headers := map[string]string{
		"k3s-Node-Name":     cfg.NodeName,
		"k3s-Node-Password": nodePassword,
	}
	if cfg.NodeIP != "" {
		headers["k3s-Node-IP"] = cfg.NodeIP
	}
	certPEM, err := bootstrapClient.Do(ctx, http.MethodPost, "/v1-k3s/client-kubelet.crt", csrPEM, "application/pkcs10", headers)
	if err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden && strings.Contains(apiErr.Body, "hash does not match") {
			return nil, nil, fmt.Errorf("request kubelet client certificate: K3s already has a different node password for %q; reuse the original --state-dir %q, or remove that Node and its kube-system/%s.node-password.k3s Secret before joining again", cfg.NodeName, cfg.StateDir, cfg.NodeName)
		}
		return nil, nil, fmt.Errorf("request kubelet client certificate: %w", err)
	}
	if block, _ := pem.Decode(certPEM); block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("k3s returned an invalid client certificate")
	}

	caFile := filepath.Join(cfg.StateDir, "server-ca.crt")
	certFile := filepath.Join(cfg.StateDir, "client-kubelet.crt")
	keyFile := filepath.Join(cfg.StateDir, "client-kubelet.key")
	passwordFile := filepath.Join(cfg.StateDir, "node-password")
	for path, file := range map[string]struct {
		body []byte
		mode os.FileMode
	}{
		caFile:       {caPEM, 0600},
		certFile:     {certPEM, 0600},
		keyFile:      {keyPEM, 0600},
		passwordFile: {[]byte(nodePassword + "\n"), 0600},
	} {
		if err := writePrivateFile(path, file.body, file.mode); err != nil {
			return nil, nil, err
		}
	}
	peerKubeconfig := expandPath(cfg.PeerKubeconfig)
	if peerKubeconfig == "" {
		candidate := defaultPeerKubeconfig()
		if candidate != "" {
			if _, statErr := os.Stat(expandPath(candidate)); statErr == nil {
				peerKubeconfig = candidate
			}
		}
	}
	state := &LocalState{
		Version: 1, Server: cfg.Server, NodeName: cfg.NodeName, NodeIP: cfg.NodeIP, ExternalIP: cfg.ExternalIP,
		PeerKubeconfig: peerKubeconfig, PeerContext: cfg.PeerContext,
		CAFile: caFile, ClientCert: certFile, ClientKey: keyFile, PasswordFile: passwordFile,
		ClientCA:    filepath.Join(cfg.StateDir, "client-ca.crt"),
		ServingCert: filepath.Join(cfg.StateDir, "serving-kubelet.crt"),
		ServingKey:  filepath.Join(cfg.StateDir, "serving-kubelet.key"),
	}
	if err := ensureKubeletServerMaterial(ctx, bootstrapClient, state, statePath, cfg.NodeName, cfg.NodeIP); err != nil {
		return nil, nil, err
	}
	client, err := newAPIClient(state.Server, caPEM, state.ClientCert, state.ClientKey, cfg.InsecureSkipTLSVerify, "", "")
	if err != nil {
		return nil, nil, err
	}
	return state, client, nil
}
