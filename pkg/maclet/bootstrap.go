package maclet

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type k3sConfigResponse struct {
	ClusterIPRanges []*net.IPNet `json:"ClusterIPRanges"`
	ClusterIPRange  *net.IPNet   `json:"ClusterIPRange"`
	ServiceIPRanges []*net.IPNet `json:"ServiceIPRanges"`
	ServiceIPRange  *net.IPNet   `json:"ServiceIPRange"`
	ClusterDNSs     []net.IP     `json:"ClusterDNSs"`
	ClusterDNS      net.IP       `json:"ClusterDNS"`
}

func firstIPv4CIDR(name string, primary *net.IPNet, ranges []*net.IPNet) (string, error) {
	// K3s prefers the plural fields when they contain entries. This matters for
	// dual-stack clusters, where the singular field may not represent the full
	// configured set.
	candidates := ranges
	if len(candidates) == 0 && primary != nil {
		candidates = []*net.IPNet{primary}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.IP.To4() == nil {
			continue
		}
		ones, bits := candidate.Mask.Size()
		if bits != 32 || ones < 0 {
			continue
		}
		return (&net.IPNet{IP: candidate.IP.To4(), Mask: net.CIDRMask(ones, 32)}).String(), nil
	}
	return "", fmt.Errorf("K3s %s configuration contains no IPv4 CIDR", name)
}

func decodeK3sAgentConfig(body []byte) (clusterCIDR, serviceCIDR string, clusterDNS []string, err error) {
	var response k3sConfigResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", "", nil, fmt.Errorf("decode k3s agent configuration: %w", err)
	}
	clusterCIDR, err = firstIPv4CIDR("cluster", response.ClusterIPRange, response.ClusterIPRanges)
	if err != nil {
		return "", "", nil, err
	}
	serviceCIDR, err = firstIPv4CIDR("service", response.ServiceIPRange, response.ServiceIPRanges)
	if err != nil {
		return "", "", nil, err
	}
	ips := response.ClusterDNSs
	if len(ips) == 0 && response.ClusterDNS != nil {
		ips = []net.IP{response.ClusterDNS}
	}
	seen := make(map[string]bool, len(ips))
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		value := ip.String()
		if net.ParseIP(value) == nil || seen[value] {
			continue
		}
		seen[value] = true
		clusterDNS = append(clusterDNS, value)
	}
	if (len(response.ClusterDNSs) > 0 || response.ClusterDNS != nil) && len(clusterDNS) == 0 {
		return "", "", nil, errors.New("K3s cluster DNS configuration contains no usable IP address")
	}
	return clusterCIDR, serviceCIDR, clusterDNS, nil
}

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
		if !validPEMCertificateFile(state.ControllerCert) && (cfg.Token != "" || cfg.TokenFile != "") {
			token, err := readToken(cfg.Token, cfg.TokenFile)
			if err != nil {
				return nil, nil, err
			}
			password, _, err := tokenPassword(token)
			if err != nil {
				return nil, nil, err
			}
			caPEM, err := os.ReadFile(state.CAFile)
			if err != nil {
				return nil, nil, fmt.Errorf("read server CA for controller certificate: %w", err)
			}
			bootstrapClient, err := newAPIClientWithMaterial(state.Server, caPEM, nil, nil, cfg.InsecureSkipTLSVerify, "node", password, "")
			if err != nil {
				return nil, nil, err
			}
			if err := ensureControllerClientMaterial(ctx, bootstrapClient, &state, statePath); err != nil {
				return nil, nil, err
			}
		}
		return &state, client, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read %s: %w", statePath, err)
	}

	if cfg.Server == "" {
		return nil, nil, errors.New("--server is required for first join")
	}
	if cfg.NodeName == "" {
		cfg.NodeName = defaultNodeName()
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
	configBody, err := bootstrapClient.Get(ctx, "/v1-k3s/config")
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve k3s agent configuration: %w", err)
	}
	clusterCIDR, serviceCIDR, clusterDNS, err := decodeK3sAgentConfig(configBody)
	if err != nil {
		return nil, nil, err
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
	state := &LocalState{
		Version: 1, Server: cfg.Server, NodeName: cfg.NodeName, NodeIP: cfg.NodeIP, ExternalIP: cfg.ExternalIP,
		ClusterCIDR: clusterCIDR, ServiceCIDR: serviceCIDR, ClusterDNS: clusterDNS,
		PeerKubeconfig: peerKubeconfig, PeerContext: cfg.PeerContext,
		CAFile: caFile, ClientCert: certFile, ClientKey: keyFile, PasswordFile: passwordFile,
		ClientCA:       filepath.Join(cfg.StateDir, "client-ca.crt"),
		ControllerCert: filepath.Join(cfg.StateDir, "client-k3s-controller.crt"),
		ControllerKey:  filepath.Join(cfg.StateDir, "client-k3s-controller.key"),
		ServingCert:    filepath.Join(cfg.StateDir, "serving-kubelet.crt"),
		ServingKey:     filepath.Join(cfg.StateDir, "serving-kubelet.key"),
	}
	if err := ensureControllerClientMaterial(ctx, bootstrapClient, state, statePath); err != nil {
		return nil, nil, err
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
