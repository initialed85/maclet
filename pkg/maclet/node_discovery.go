package maclet

import (
	"fmt"
	"net"
	"net/url"
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

func peerAPIClient(cfg JoinConfig, state *LocalState) (*APIClient, error) {
	path := cfg.PeerKubeconfig
	if path == "" {
		path = state.PeerKubeconfig
	}
	if path == "" {
		path = defaultPeerKubeconfig()
	}
	contextName := cfg.PeerContext
	if contextName == "" {
		contextName = state.PeerContext
	}
	client, found, err := loadPeerAPIClient(state.Server, path, contextName, cfg.InsecureSkipTLSVerify)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("peer Node discovery needs a readable kubeconfig at %s; pass --peer-kubeconfig or use --vxlan-gateway-mac", path)
	}
	return client, nil
}
