package maclet

import "time"

type LocalState struct {
	Version        int      `json:"version"`
	Server         string   `json:"server"`
	NodeName       string   `json:"nodeName"`
	NodeIP         string   `json:"nodeIP"`
	InstanceID     string   `json:"instanceID,omitempty"`
	NodeUID        string   `json:"nodeUID,omitempty"`
	ExternalIP     string   `json:"externalIP,omitempty"`
	PeerKubeconfig string   `json:"peerKubeconfig,omitempty"`
	PeerContext    string   `json:"peerContext,omitempty"`
	CAFile         string   `json:"caFile"`
	ClientCert     string   `json:"clientCert"`
	ClientKey      string   `json:"clientKey"`
	ClientCA       string   `json:"clientCA,omitempty"`
	ControllerCert string   `json:"controllerCert,omitempty"`
	ControllerKey  string   `json:"controllerKey,omitempty"`
	ClusterCIDR    string   `json:"clusterCIDR,omitempty"`
	ServiceCIDR    string   `json:"serviceCIDR,omitempty"`
	ClusterDNS     []string `json:"clusterDNS,omitempty"`
	ServingCert    string   `json:"servingCert,omitempty"`
	ServingKey     string   `json:"servingKey,omitempty"`
	PasswordFile   string   `json:"passwordFile"`
}

type JoinConfig struct {
	Server                string
	Token                 string
	TokenFile             string
	NodeName              string
	NodeIP                string
	Adopt                 bool
	ExternalIP            string
	StateDir              string
	InsecureSkipTLSVerify bool
	Once                  bool
	VXLANBinary           string
	MackerBinary          string
	Debug                 bool
	DNSResolver           bool
	VXLANRemote           string
	VXLANLocal            string
	VXLANGatewayMAC       string
	PeerKubeconfig        string
	PeerContext           string
	DrainTimeout          time.Duration
	VXLANPort             int
	VXLANMTU              int
	ClusterCIDR           string
	ServiceCIDR           string
	clusterCIDRSet        bool
	serviceCIDRSet        bool
	useSudo               bool
}

type VXLANHandle struct {
	BridgeCIDR string
	BridgeName string
	BridgeMAC  string
	cleanup    func()
}

type DarwinRoute struct {
	Network string
	Netmask string
	Gateway string
}

type FlannelPeer struct {
	NodeName string
	PodCIDR  string
	PublicIP string
	VtepMAC  string
}

type DarwinPeerGateway struct {
	PodCIDR  string
	Gateway  string
	MAC      string
	PublicIP string
}

type DarwinARPEntry struct {
	IP  string
	MAC string
}

type DarwinNetworkHandle struct {
	Interface    string
	PodCIDR      string
	Gateway      string
	GatewayMAC   string
	PeerGateways []DarwinPeerGateway
	ARPs         []DarwinARPEntry
	Routes       []DarwinRoute
	Aliases      []string
	useSudo      bool
}
