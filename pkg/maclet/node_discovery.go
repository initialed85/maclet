package maclet

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func detectNodeIP(server string) string {
	u, err := url.Parse(server)
	if err == nil {
		host := u.Hostname()
		if connection, err := net.DialTimeout("udp", net.JoinHostPort(host, "6443"), time.Second); err == nil {
			defer connection.Close()
			if address, ok := connection.LocalAddr().(*net.UDPAddr); ok {
				return address.IP.String()
			}
		}
	}
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		if ipNet, ok := address.(*net.IPNet); ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() {
			return ipNet.IP.String()
		}
	}
	return "127.0.0.1"
}

func vxlanPublicIP(cfg JoinConfig, state *LocalState) string {
	if cfg.VXLANLocal != "" {
		return cfg.VXLANLocal
	}
	return state.NodeIP
}

func configuredPeerAPIClient(cfg JoinConfig, state *LocalState) (*APIClient, bool, error) {
	path := cfg.PeerKubeconfig
	if path == "" {
		path = state.PeerKubeconfig
	}
	if path == "" {
		return nil, false, nil
	}
	contextName := cfg.PeerContext
	if contextName == "" {
		contextName = state.PeerContext
	}
	client, found, err := loadPeerAPIClient(state.Server, path, contextName, cfg.InsecureSkipTLSVerify)
	if err != nil {
		return nil, false, err
	}
	return client, found, nil
}

func peerAPIClient(cfg JoinConfig, state *LocalState) (*APIClient, error) {
	// An explicitly supplied kubeconfig remains an escape hatch for clusters
	// where the standard K3s controller certificate is unavailable. New joins
	// use the controller certificate obtained with the join token instead.
	if client, found, err := configuredPeerAPIClient(cfg, state); err != nil {
		return nil, err
	} else if found {
		return client, nil
	}

	certFile := state.ControllerCert
	keyFile := state.ControllerKey
	if certFile == "" {
		certFile = filepath.Join(filepath.Dir(state.CAFile), "client-k3s-controller.crt")
	}
	if keyFile == "" {
		keyFile = filepath.Join(filepath.Dir(state.CAFile), "client-k3s-controller.key")
	}
	if validPEMCertificateFile(certFile) {
		if _, err := os.Stat(keyFile); err == nil {
			caPEM, err := os.ReadFile(state.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read K3s controller client CA: %w", err)
			}
			client, err := newAPIClient(state.Server, caPEM, certFile, keyFile, cfg.InsecureSkipTLSVerify, "", "")
			if err != nil {
				return nil, fmt.Errorf("create K3s controller API client: %w", err)
			}
			return client, nil
		}
	}
	return nil, fmt.Errorf("automatic peer discovery needs the K3s controller client certificate; rerun join with the original token, pass --peer-kubeconfig, or use --vxlan-gateway-mac")
}
