package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	version                  = "0.1.0-dev"
	defaultStateDir          = ".maclet"
	defaultNodeName          = "maclet"
	defaultVXLANPort         = 8472
	defaultVXLANMTU          = 1450
	defaultClusterCIDR       = "10.42.0.0/16"
	defaultServiceCIDR       = "10.43.0.0/16"
	defaultMaxPods           = 110
	defaultLeaseDurationSecs = 40
	defaultHeartbeat         = 10 * time.Second
	defaultDrainTimeout      = 10 * time.Second
)

var errNotFound = errors.New("resource not found")

// APIClient is the small HTTPS client used by maclet. It deliberately uses
// only the Kubernetes JSON API rather than pulling in client-go: maclet needs a
// narrow, inspectable control-plane surface while its runtime is still being
// designed.
type APIClient struct {
	base        *url.URL
	http        *http.Client
	username    string
	password    string
	bearerToken string
}

type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("kubernetes API returned HTTP %d", e.Code)
	}
	return fmt.Sprintf("kubernetes API returned HTTP %d: %s", e.Code, e.Body)
}

func newAPIClient(server string, caPEM []byte, certFile, keyFile string, insecure bool, username, password string) (*APIClient, error) {
	var certPEM, keyPEM []byte
	if certFile != "" || keyFile != "" {
		var err error
		certPEM, err = os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("read client certificate: %w", err)
		}
		keyPEM, err = os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read client key: %w", err)
		}
	}
	return newAPIClientWithMaterial(server, caPEM, certPEM, keyPEM, insecure, username, password, "")
}

func newAPIClientWithMaterial(server string, caPEM, certPEM, keyPEM []byte, insecure bool, username, password, bearerToken string) (*APIClient, error) {
	base, err := normalizeServer(server)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsConfig.InsecureSkipVerify = true // explicitly requested for development clusters
	} else {
		pool := x509.NewCertPool()
		if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("cluster CA bundle is empty or invalid")
		}
		tlsConfig.RootCAs = pool
	}
	if len(certPEM) != 0 || len(keyPEM) != 0 {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return &APIClient{
		base: base,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		username:    username,
		password:    password,
		bearerToken: bearerToken,
	}, nil
}

func normalizeServer(server string) (*url.URL, error) {
	if server == "" {
		return nil, errors.New("server URL is required")
	}
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("server URL must use https:// (got %q)", server)
	}
	if u.Host == "" {
		return nil, errors.New("server URL has no host")
	}
	return u, nil
}

func (c *APIClient) endpoint(path string) string {
	u := *c.base
	p, err := url.Parse(path)
	if err != nil {
		return ""
	}
	u.Path = p.Path
	u.RawQuery = p.RawQuery
	u.Fragment = ""
	return u.String()
}

func (c *APIClient) do(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	} else if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Code: response.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	return responseBody, nil
}

func (c *APIClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, nil, "", nil)
}

func (c *APIClient) postJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, path, body, "application/json", nil)
}

func (c *APIClient) putJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, path, body, "application/json", nil)
}

func (c *APIClient) patchJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPatch, path, body, "application/merge-patch+json", nil)
}

type kubeconfigJSON struct {
	CurrentContext string `json:"current-context"`
	Clusters       []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server                   string `json:"server"`
			CertificateAuthority     string `json:"certificate-authority"`
			CertificateAuthorityData string `json:"certificate-authority-data"`
		} `json:"cluster"`
	} `json:"clusters"`
	Contexts []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster string `json:"cluster"`
			User    string `json:"user"`
		} `json:"context"`
	} `json:"contexts"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token                 string `json:"token"`
			TokenFile             string `json:"tokenFile"`
			Username              string `json:"username"`
			Password              string `json:"password"`
			ClientCertificate     string `json:"client-certificate"`
			ClientCertificateData string `json:"client-certificate-data"`
			ClientKey             string `json:"client-key"`
			ClientKeyData         string `json:"client-key-data"`
		} `json:"user"`
	} `json:"users"`
}

func homeDirectory() string {
	if os.Geteuid() == 0 {
		if sudoHome := os.Getenv("SUDO_HOME"); sudoHome != "" {
			return sudoHome
		}
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			if account, err := osuser.Lookup(sudoUser); err == nil && account.HomeDir != "" {
				return account.HomeDir
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func defaultPeerKubeconfig() string {
	if value := os.Getenv("KUBECONFIG"); value != "" {
		return value
	}
	home := homeDirectory()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func expandPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home := homeDirectory(); home != "" {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func decodeKubeconfigData(value, description string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	body, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", description, err)
	}
	return body, nil
}

func kubeconfigFile(baseDir, path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	path = expandPath(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig file %s: %w", path, err)
	}
	return body, nil
}

func loadPeerAPIClient(server, kubeconfigPath, contextName string, insecure bool) (*APIClient, bool, error) {
	if kubeconfigPath == "" {
		return nil, false, nil
	}
	kubeconfigPath = expandPath(kubeconfigPath)
	if _, err := os.Stat(kubeconfigPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat peer kubeconfig: %w", err)
	}
	command := exec.Command("kubectl", "config", "view", "--raw", "--output=json", "--kubeconfig", kubeconfigPath)
	if contextName != "" {
		command.Args = append(command.Args, "--context", contextName)
	}
	body, err := command.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, false, fmt.Errorf("read peer kubeconfig with kubectl: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, false, fmt.Errorf("read peer kubeconfig with kubectl: %w", err)
	}
	var config kubeconfigJSON
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, false, fmt.Errorf("decode peer kubeconfig: %w", err)
	}
	if contextName == "" {
		contextName = config.CurrentContext
	}
	var selectedContext struct {
		cluster string
		user    string
	}
	for _, candidate := range config.Contexts {
		if candidate.Name == contextName {
			selectedContext.cluster = candidate.Context.Cluster
			selectedContext.user = candidate.Context.User
			break
		}
	}
	if selectedContext.cluster == "" || selectedContext.user == "" {
		return nil, false, fmt.Errorf("peer kubeconfig context %q is missing or incomplete", contextName)
	}
	var cluster struct {
		server                   string
		certificateAuthority     string
		certificateAuthorityData string
	}
	for _, candidate := range config.Clusters {
		if candidate.Name == selectedContext.cluster {
			cluster.server = candidate.Cluster.Server
			cluster.certificateAuthority = candidate.Cluster.CertificateAuthority
			cluster.certificateAuthorityData = candidate.Cluster.CertificateAuthorityData
			break
		}
	}
	if cluster.server == "" {
		return nil, false, fmt.Errorf("peer kubeconfig cluster %q is missing a server", selectedContext.cluster)
	}
	var selectedUser struct {
		token                 string
		tokenFile             string
		username              string
		password              string
		clientCertificate     string
		clientCertificateData string
		clientKey             string
		clientKeyData         string
	}
	for _, candidate := range config.Users {
		if candidate.Name == selectedContext.user {
			selectedUser.token = candidate.User.Token
			selectedUser.tokenFile = candidate.User.TokenFile
			selectedUser.username = candidate.User.Username
			selectedUser.password = candidate.User.Password
			selectedUser.clientCertificate = candidate.User.ClientCertificate
			selectedUser.clientCertificateData = candidate.User.ClientCertificateData
			selectedUser.clientKey = candidate.User.ClientKey
			selectedUser.clientKeyData = candidate.User.ClientKeyData
			break
		}
	}
	baseDir := filepath.Dir(kubeconfigPath)
	caPEM, err := decodeKubeconfigData(cluster.certificateAuthorityData, "peer certificate-authority-data")
	if err != nil {
		return nil, false, err
	}
	if len(caPEM) == 0 {
		caPEM, err = kubeconfigFile(baseDir, cluster.certificateAuthority)
		if err != nil {
			return nil, false, err
		}
	}
	certPEM, err := decodeKubeconfigData(selectedUser.clientCertificateData, "peer client-certificate-data")
	if err != nil {
		return nil, false, err
	}
	if len(certPEM) == 0 {
		certPEM, err = kubeconfigFile(baseDir, selectedUser.clientCertificate)
		if err != nil {
			return nil, false, err
		}
	}
	keyPEM, err := decodeKubeconfigData(selectedUser.clientKeyData, "peer client-key-data")
	if err != nil {
		return nil, false, err
	}
	if len(keyPEM) == 0 {
		keyPEM, err = kubeconfigFile(baseDir, selectedUser.clientKey)
		if err != nil {
			return nil, false, err
		}
	}
	token := selectedUser.token
	if token == "" && selectedUser.tokenFile != "" {
		body, err := kubeconfigFile(baseDir, selectedUser.tokenFile)
		if err != nil {
			return nil, false, err
		}
		token = strings.TrimSpace(string(body))
	}
	if len(certPEM) == 0 && token == "" && selectedUser.username == "" {
		return nil, false, errors.New("peer kubeconfig user has no token, username/password, or client certificate")
	}
	client, err := newAPIClientWithMaterial(server, caPEM, certPEM, keyPEM, insecure, selectedUser.username, selectedUser.password, token)
	if err != nil {
		return nil, false, fmt.Errorf("create peer API client: %w", err)
	}
	return client, true, nil
}

// Kubernetes object fragments used by the registration and workload paths.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

type Taint struct {
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Effect    string `json:"effect"`
	TimeAdded string `json:"timeAdded,omitempty"`
}

type NodeSpec struct {
	PodCIDR       string   `json:"podCIDR,omitempty"`
	PodCIDRs      []string `json:"podCIDRs,omitempty"`
	Taints        []Taint  `json:"taints,omitempty"`
	Unschedulable bool     `json:"unschedulable,omitempty"`
}

type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

type NodeCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastHeartbeatTime  string `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

type NodeInfo struct {
	Architecture            string `json:"architecture,omitempty"`
	OperatingSystem         string `json:"operatingSystem,omitempty"`
	KernelVersion           string `json:"kernelVersion,omitempty"`
	OSImage                 string `json:"osImage,omitempty"`
	ContainerRuntimeVersion string `json:"containerRuntimeVersion,omitempty"`
	KubeletVersion          string `json:"kubeletVersion,omitempty"`
	KubeProxyVersion        string `json:"kubeProxyVersion,omitempty"`
}

type NodeStatus struct {
	Addresses       []NodeAddress     `json:"addresses,omitempty"`
	Conditions      []NodeCondition   `json:"conditions,omitempty"`
	NodeInfo        NodeInfo          `json:"nodeInfo,omitempty"`
	DaemonEndpoints any               `json:"daemonEndpoints,omitempty"`
	Capacity        map[string]string `json:"capacity,omitempty"`
	Allocatable     map[string]string `json:"allocatable,omitempty"`
}

type Node struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       NodeSpec   `json:"spec"`
	Status     NodeStatus `json:"status,omitempty"`
}

type NodeList struct {
	Items []Node `json:"items"`
}

type Lease struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       LeaseSpec  `json:"spec"`
}

type LeaseSpec struct {
	HolderIdentity       string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds int32  `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	RenewTime            string `json:"renewTime,omitempty"`
	LeaseTransitions     int32  `json:"leaseTransitions,omitempty"`
}

type PodList struct {
	Items []Pod `json:"items"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}

type PodSpec struct {
	NodeName       string          `json:"nodeName,omitempty"`
	RestartPolicy  string          `json:"restartPolicy,omitempty"`
	Volumes        []Volume        `json:"volumes,omitempty"`
	Containers     []ContainerSpec `json:"containers,omitempty"`
	InitContainers []ContainerSpec `json:"initContainers,omitempty"`
}

type Volume struct {
	Name     string                `json:"name,omitempty"`
	HostPath *HostPathVolumeSource `json:"hostPath,omitempty"`
}

type HostPathVolumeSource struct {
	Path string `json:"path,omitempty"`
	Type string `json:"type,omitempty"`
}

type ContainerSpec struct {
	Name         string          `json:"name"`
	Image        string          `json:"image,omitempty"`
	Command      []string        `json:"command,omitempty"`
	Args         []string        `json:"args,omitempty"`
	WorkingDir   string          `json:"workingDir,omitempty"`
	Env          []EnvVar        `json:"env,omitempty"`
	Ports        []ContainerPort `json:"ports,omitempty"`
	VolumeMounts []VolumeMount   `json:"volumeMounts,omitempty"`
}

type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	HostPort      int32  `json:"hostPort,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

type EnvVarSource struct {
	FieldRef *ObjectFieldSelector `json:"fieldRef,omitempty"`
}

type ObjectFieldSelector struct {
	FieldPath string `json:"fieldPath,omitempty"`
}

type VolumeMount struct {
	Name        string `json:"name,omitempty"`
	MountPath   string `json:"mountPath,omitempty"`
	SubPath     string `json:"subPath,omitempty"`
	SubPathExpr string `json:"subPathExpr,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
}

type PodIP struct {
	IP string `json:"ip"`
}

type PodCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastProbeTime      string `json:"lastProbeTime,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type ContainerStateRunning struct {
	StartedAt string `json:"startedAt,omitempty"`
}

type ContainerStateTerminated struct {
	ExitCode   int32  `json:"exitCode"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	State        ContainerState `json:"state"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	Image        string         `json:"image,omitempty"`
}

type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	PodIP                 string            `json:"podIP,omitempty"`
	PodIPs                []PodIP           `json:"podIPs,omitempty"`
	HostIP                string            `json:"hostIP,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	Message               string            `json:"message,omitempty"`
}

type LocalState struct {
	Version        int    `json:"version"`
	Server         string `json:"server"`
	NodeName       string `json:"nodeName"`
	NodeIP         string `json:"nodeIP"`
	ExternalIP     string `json:"externalIP,omitempty"`
	PeerKubeconfig string `json:"peerKubeconfig,omitempty"`
	PeerContext    string `json:"peerContext,omitempty"`
	CAFile         string `json:"caFile"`
	ClientCert     string `json:"clientCert"`
	ClientKey      string `json:"clientKey"`
	PasswordFile   string `json:"passwordFile"`
}

type JoinConfig struct {
	Server                string
	Token                 string
	TokenFile             string
	NodeName              string
	NodeIP                string
	ExternalIP            string
	StateDir              string
	InsecureSkipTLSVerify bool
	Once                  bool
	VXLANBinary           string
	MackerBinary          string
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

const (
	managedLabelKey    = "k8s-darwin.dev/native"
	managedLabelValue  = "true"
	managedTaintKey    = "k8s-darwin.dev/native"
	managedTaintValue  = "true"
	managedTaintEffect = "NoSchedule"

	flannelBackendTypeAnnotation   = "flannel.alpha.coreos.com/backend-type"
	flannelBackendDataAnnotation   = "flannel.alpha.coreos.com/backend-data"
	flannelPublicIPAnnotation      = "flannel.alpha.coreos.com/public-ip"
	flannelSubnetManagerAnnotation = "flannel.alpha.coreos.com/kube-subnet-manager"
	shutdownCordonAnnotation       = "k8s-darwin.dev/maclet-shutdown-cordon"
)

func desiredNode(name, nodeIP string) Node {
	labels := map[string]string{
		"kubernetes.io/arch": "arm64",
		"kubernetes.io/os":   "darwin",
		managedLabelKey:      managedLabelValue,
	}
	if nodeIP == "" {
		nodeIP = "127.0.0.1"
	}
	return Node{
		APIVersion: "v1",
		Kind:       "Node",
		Metadata: ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: NodeSpec{Taints: []Taint{{
			Key: managedTaintKey, Value: managedTaintValue, Effect: managedTaintEffect,
		}}},
	}
}

func hasManagedTaint(taints []Taint) bool {
	for _, taint := range taints {
		if taint.Key == managedTaintKey && taint.Value == managedTaintValue && taint.Effect == managedTaintEffect {
			return true
		}
	}
	return false
}

func setNodeShutdownCordon(ctx context.Context, client *APIClient, node *Node, cordon bool) (*Node, error) {
	marker := ""
	if cordon {
		marker = "true"
	}
	alreadyMarked := node.Metadata.Annotations[shutdownCordonAnnotation] == "true"
	if cordon && node.Spec.Unschedulable && alreadyMarked {
		return node, nil
	}
	if !cordon && !alreadyMarked {
		return node, nil
	}
	path := "/api/v1/nodes/" + url.PathEscape(node.Metadata.Name)
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		annotations := map[string]any{shutdownCordonAnnotation: marker}
		if !cordon {
			annotations[shutdownCordonAnnotation] = nil
		}
		body, err := client.patchJSON(ctx, path, map[string]any{
			"metadata": map[string]any{"annotations": annotations},
			"spec":     map[string]any{"unschedulable": cordon},
		})
		if err == nil {
			var updated Node
			if decodeErr := json.Unmarshal(body, &updated); decodeErr != nil {
				return nil, fmt.Errorf("decode Node shutdown cordon update: %w", decodeErr)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(err, &conflict) || conflict.Code != http.StatusConflict || attempt == 4 {
			return nil, fmt.Errorf("set Node %q shutdown cordon=%t: %w", current.Metadata.Name, cordon, err)
		}
		body, err = client.get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("refresh Node after shutdown cordon conflict: %w", err)
		}
		var refreshed Node
		if err := json.Unmarshal(body, &refreshed); err != nil {
			return nil, fmt.Errorf("decode Node after shutdown cordon conflict: %w", err)
		}
		current = &refreshed
		alreadyMarked = current.Metadata.Annotations[shutdownCordonAnnotation] == "true"
		if (cordon && current.Spec.Unschedulable && alreadyMarked) || (!cordon && !alreadyMarked) {
			return current, nil
		}
	}
	return nil, errors.New("Node shutdown cordon update retry limit exceeded")
}

func ensureNode(ctx context.Context, client *APIClient, name, nodeIP string) (*Node, error) {
	path := "/api/v1/nodes/" + url.PathEscape(name)
	body, err := client.get(ctx, path)
	if err != nil {
		var apiErr *HTTPError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
			return nil, err
		}
		desired := desiredNode(name, nodeIP)
		body, err = client.postJSON(ctx, "/api/v1/nodes", desired)
		if err != nil {
			var conflict *HTTPError
			if errors.As(err, &conflict) && conflict.Code == http.StatusConflict {
				body, err = client.get(ctx, path)
			} else {
				return nil, fmt.Errorf("create Node %q: %w", name, err)
			}
		}
	}
	var node Node
	if err := json.Unmarshal(body, &node); err != nil {
		return nil, fmt.Errorf("decode Node %q: %w", name, err)
	}

	labels := node.Metadata.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	patchLabels := map[string]string{}
	for key, value := range desiredNode(name, nodeIP).Metadata.Labels {
		if labels[key] != value {
			patchLabels[key] = value
		}
	}
	needsPatch := len(patchLabels) > 0 || !hasManagedTaint(node.Spec.Taints)
	if needsPatch {
		mergedTaints := append([]Taint(nil), node.Spec.Taints...)
		if !hasManagedTaint(mergedTaints) {
			mergedTaints = append(mergedTaints, Taint{Key: managedTaintKey, Value: managedTaintValue, Effect: managedTaintEffect})
		}
		mergedLabels := make(map[string]string, len(labels)+len(patchLabels))
		for key, value := range labels {
			mergedLabels[key] = value
		}
		for key, value := range desiredNode(name, nodeIP).Metadata.Labels {
			mergedLabels[key] = value
		}
		patch := map[string]any{
			"metadata": map[string]any{
				"labels": mergedLabels,
			},
			"spec": map[string]any{"taints": mergedTaints},
		}
		body, err = client.patchJSON(ctx, path, patch)
		if err != nil {
			return nil, fmt.Errorf("patch Node %q: %w", name, err)
		}
		if err := json.Unmarshal(body, &node); err != nil {
			return nil, fmt.Errorf("decode patched Node %q: %w", name, err)
		}
	}
	return &node, nil
}

func nodeStatus(name, nodeIP, externalIP string, now time.Time) NodeStatus {
	stamp := now.UTC().Format(time.RFC3339Nano)
	addresses := []NodeAddress{
		{Type: "InternalIP", Address: nodeIP},
	}
	if externalIP != "" {
		addresses = append(addresses, NodeAddress{Type: "ExternalIP", Address: externalIP})
	}
	addresses = append(addresses, NodeAddress{Type: "Hostname", Address: name})
	kernel := "unknown"
	if output, err := exec.Command("uname", "-r").Output(); err == nil {
		kernel = strings.TrimSpace(string(output))
	}
	osImage := runtime.GOOS
	if output, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		osImage = "macOS " + strings.TrimSpace(string(output))
	}
	capacity := map[string]string{"pods": strconv.Itoa(defaultMaxPods)}
	allocatable := map[string]string{"pods": strconv.Itoa(defaultMaxPods)}
	return NodeStatus{
		Addresses:   addresses,
		Capacity:    capacity,
		Allocatable: allocatable,
		Conditions: []NodeCondition{
			{Type: "MemoryPressure", Status: "False", LastHeartbeatTime: stamp, LastTransitionTime: stamp, Reason: "MacletHasSufficientMemory", Message: "maclet does not report memory pressure"},
			{Type: "DiskPressure", Status: "False", LastHeartbeatTime: stamp, LastTransitionTime: stamp, Reason: "MacletHasNoDiskPressure", Message: "maclet does not report disk pressure"},
			{Type: "PIDPressure", Status: "False", LastHeartbeatTime: stamp, LastTransitionTime: stamp, Reason: "MacletHasSufficientPID", Message: "maclet does not report PID pressure"},
			{Type: "Ready", Status: "True", LastHeartbeatTime: stamp, LastTransitionTime: stamp, Reason: "MacletReady", Message: "maclet is supervising trusted native Darwin workloads"},
		},
		NodeInfo: NodeInfo{
			Architecture:            "arm64",
			OperatingSystem:         "darwin",
			KernelVersion:           kernel,
			OSImage:                 osImage,
			ContainerRuntimeVersion: "macker://trusted-native",
			KubeletVersion:          "maclet/" + version,
			KubeProxyVersion:        "maclet/" + version,
		},
	}
}

func updateNodeStatus(ctx context.Context, client *APIClient, node *Node, nodeIP, externalIP string) (*Node, error) {
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		status := nodeStatus(current.Metadata.Name, nodeIP, externalIP, time.Now())
		payload := map[string]any{
			"apiVersion": "v1",
			"kind":       "Node",
			"metadata": map[string]any{
				"name":            current.Metadata.Name,
				"resourceVersion": current.Metadata.ResourceVersion,
				"labels":          current.Metadata.Labels,
				"annotations":     current.Metadata.Annotations,
			},
			// Include the current spec on status updates. NodeRestriction compares
			// the submitted object as a whole and must see the existing taint and
			// controller-assigned PodCIDR rather than an omitted/empty spec.
			"spec":   current.Spec,
			"status": status,
		}
		body, err := client.putJSON(ctx, "/api/v1/nodes/"+url.PathEscape(current.Metadata.Name)+"/status", payload)
		if err == nil {
			var updated Node
			if err := json.Unmarshal(body, &updated); err != nil {
				return nil, fmt.Errorf("decode Node status response: %w", err)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(err, &conflict) || conflict.Code != http.StatusConflict || attempt == 4 {
			return nil, fmt.Errorf("update Node %q status: %w", current.Metadata.Name, err)
		}
		latest, getErr := client.get(ctx, "/api/v1/nodes/"+url.PathEscape(current.Metadata.Name))
		if getErr != nil {
			return nil, fmt.Errorf("refresh Node after status conflict: %w", getErr)
		}
		var refreshed Node
		if getErr := json.Unmarshal(latest, &refreshed); getErr != nil {
			return nil, fmt.Errorf("decode Node after status conflict: %w", getErr)
		}
		current = &refreshed
	}
	return nil, errors.New("status update retry limit exceeded")
}

func ensureLease(ctx context.Context, client *APIClient, nodeName string) error {
	path := "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/" + url.PathEscape(nodeName)
	// Kubernetes' metav1.Time decoder on this cluster expects six fractional
	// second digits for Lease timestamps.
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	body, err := client.get(ctx, path)
	lease := Lease{
		APIVersion: "coordination.k8s.io/v1",
		Kind:       "Lease",
		Metadata:   ObjectMeta{Name: nodeName, Namespace: "kube-node-lease"},
		Spec: LeaseSpec{
			HolderIdentity:       nodeName,
			LeaseDurationSeconds: defaultLeaseDurationSecs,
			AcquireTime:          now,
			RenewTime:            now,
		},
	}
	if err != nil {
		var apiErr *HTTPError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
			return err
		}
		if _, err := client.postJSON(ctx, "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases", lease); err != nil {
			var conflict *HTTPError
			if errors.As(err, &conflict) && conflict.Code == http.StatusConflict {
				return ensureLease(ctx, client, nodeName)
			}
			return fmt.Errorf("create node Lease: %w", err)
		}
		return nil
	}
	if err := json.Unmarshal(body, &lease); err != nil {
		return fmt.Errorf("decode node Lease: %w", err)
	}
	lease.Spec.HolderIdentity = nodeName
	lease.Spec.LeaseDurationSeconds = defaultLeaseDurationSecs
	lease.Spec.RenewTime = now
	if _, err := client.putJSON(ctx, path, lease); err != nil {
		return fmt.Errorf("renew node Lease: %w", err)
	}
	return nil
}

func waitForPodCIDR(ctx context.Context, client *APIClient, node *Node, timeout time.Duration) (*Node, error) {
	if node.Spec.PodCIDR != "" || len(node.Spec.PodCIDRs) > 0 {
		return node, nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		body, err := client.get(ctx, "/api/v1/nodes/"+url.PathEscape(node.Metadata.Name))
		if err != nil {
			return nil, err
		}
		var current Node
		if err := json.Unmarshal(body, &current); err != nil {
			return nil, err
		}
		if current.Spec.PodCIDR != "" || len(current.Spec.PodCIDRs) > 0 {
			return &current, nil
		}
		node = &current
	}
	return node, nil
}

func readToken(value, file string) (string, error) {
	if value != "" && file != "" {
		return "", errors.New("use only one of --token and --token-file")
	}
	if file != "" {
		var body []byte
		var err error
		if file == "-" {
			body, err = io.ReadAll(os.Stdin)
		} else {
			body, err = os.ReadFile(file)
		}
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		value = string(body)
	}
	if value == "" {
		value = os.Getenv("MACLET_TOKEN")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("cluster token is required (--token-file, --token, or MACLET_TOKEN)")
	}
	return value, nil
}

func tokenPassword(token string) (string, string, error) {
	credentials := token
	caHash := ""
	if strings.HasPrefix(credentials, "K10") {
		if separator := strings.Index(credentials, "::"); separator >= 0 {
			if separator >= 3+64 {
				caHash = credentials[3 : 3+64]
			}
			credentials = credentials[separator+2:]
		}
	}
	if separator := strings.IndexByte(credentials, ':'); separator >= 0 {
		credentials = credentials[separator+1:]
	}
	if credentials == "" {
		return "", caHash, errors.New("cluster token does not contain a password")
	}
	return credentials, caHash, nil
}

func fetchCA(ctx context.Context, server string, expectedHash string) ([]byte, error) {
	base, err := normalizeServer(server)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.ResolveReference(&url.URL{Path: "/cacerts"}).String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get cluster CA: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get cluster CA: HTTP %s", response.Status)
	}
	caPEM, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if _, rest := pem.Decode(caPEM); rest == nil {
		return nil, errors.New("cluster CA response is not PEM")
	}
	if expectedHash != "" {
		digest := sha256.Sum256(caPEM)
		actual := hex.EncodeToString(digest[:])
		if actual != expectedHash {
			return nil, fmt.Errorf("cluster CA hash mismatch: token=%s server=%s", expectedHash, actual)
		}
	}
	return caPEM, nil
}

func randomPassword() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func generateClientCSR(nodeName string) (csrDER, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "system:node:" + nodeName},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return requestDER,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
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
	if _, err := bootstrapClient.get(ctx, "/v1-k3s/readyz"); err != nil {
		return nil, nil, fmt.Errorf("authenticate with k3s agent token: %w", err)
	}
	if _, err := bootstrapClient.get(ctx, "/v1-k3s/config"); err != nil {
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
	certPEM, err := bootstrapClient.do(ctx, http.MethodPost, "/v1-k3s/client-kubelet.crt", csrPEM, "application/pkcs10", headers)
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
	state := &LocalState{Version: 1, Server: cfg.Server, NodeName: cfg.NodeName, NodeIP: cfg.NodeIP, ExternalIP: cfg.ExternalIP, PeerKubeconfig: peerKubeconfig, PeerContext: cfg.PeerContext, CAFile: caFile, ClientCert: certFile, ClientKey: keyFile, PasswordFile: passwordFile}
	stateBody, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := writePrivateFile(statePath, append(stateBody, '\n'), 0600); err != nil {
		return nil, nil, err
	}
	client, err := newAPIClient(state.Server, caPEM, state.ClientCert, state.ClientKey, cfg.InsecureSkipTLSVerify, "", "")
	if err != nil {
		return nil, nil, err
	}
	return state, client, nil
}

func mustReadFile(path string) []byte {
	body, _ := os.ReadFile(path)
	return body
}

func invokingOwner() (int, int, bool, error) {
	if os.Geteuid() != 0 || os.Getenv("SUDO_UID") == "" || os.Getenv("SUDO_GID") == "" {
		return 0, 0, false, nil
	}
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse SUDO_UID: %w", err)
	}
	gid, err := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse SUDO_GID: %w", err)
	}
	return uid, gid, true, nil
}

func chownToInvokingUser(path string) error {
	uid, gid, needed, err := invokingOwner()
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("set owner on %s: %w", path, err)
	}
	return nil
}

func writePrivateFile(path string, body []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".maclet-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	if err := chownToInvokingUser(path); err != nil {
		return err
	}
	return nil
}

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

func privilegedCommand(useSudo bool, name string, args ...string) *exec.Cmd {
	if useSudo {
		args = append([]string{"-n", name}, args...)
		return exec.Command("sudo", args...)
	}
	return exec.Command(name, args...)
}

func preparePrivileges(cfg *JoinConfig) error {
	if cfg.VXLANBinary == "" || os.Geteuid() == 0 {
		return nil
	}
	command := exec.Command("sudo", "-n", "true")
	if err := command.Run(); err != nil {
		return errors.New("VXLAN and Darwin route/ARP setup require root or passwordless sudo; rerun maclet join via sudo")
	}
	cfg.useSudo = true
	return nil
}

func startVXLAN(ctx context.Context, cfg JoinConfig, node *Node, peers []FlannelPeer) (*VXLANHandle, error) {
	if cfg.VXLANBinary == "" {
		return nil, nil
	}
	if cfg.VXLANRemote == "" {
		return nil, errors.New("--vxlan-remote is required when --vxlan-binary is set")
	}
	cidr := node.Spec.PodCIDR
	if cidr == "" && len(node.Spec.PodCIDRs) > 0 {
		cidr = node.Spec.PodCIDRs[0]
	}
	if cidr == "" {
		return nil, errors.New("cannot start VXLAN until Kubernetes assigns this node a PodCIDR")
	}
	bridgeCIDR, err := bridgeAddressForCIDR(cidr)
	if err != nil {
		return nil, err
	}
	local := cfg.VXLANLocal
	if local == "" {
		local = cfg.NodeIP
	}
	if local == "" {
		return nil, errors.New("cannot start VXLAN without a local underlay address")
	}
	arguments := []string{
		"--vni", "1",
		"--local", local,
		// Keep the selected peer as a fallback for service traffic and for
		// older darwin-vxlan binaries. New binaries use the peer MAC mappings
		// below for destination-specific PodCIDR traffic.
		"--remote", cfg.VXLANRemote,
		"--port", fmt.Sprint(cfg.VXLANPort),
		"--mtu", fmt.Sprint(cfg.VXLANMTU),
		"--bridge-ipv4", bridgeCIDR,
	}
	for _, peer := range peers {
		// maclet installs per-PodCIDR synthetic gateways with each peer's
		// VtepMAC. darwin-vxlan uses the inner destination IP to select this
		// peer's underlay endpoint while preserving the Ethernet frame.
		arguments = append(arguments, "--peer", peer.PodCIDR+"="+peer.PublicIP)
	}
	// ClusterIP traffic must use the selected Linux node as its service
	// gateway; kube-proxy on that node can then select the actual backend.
	arguments = append(arguments, "--peer", cfg.ServiceCIDR+"="+cfg.VXLANRemote)
	if existingBridge, err := interfaceForAddress(bridgeCIDR); err != nil {
		return nil, fmt.Errorf("inspect existing VXLAN bridge address: %w", err)
	} else if existingBridge != "" {
		return nil, fmt.Errorf("VXLAN bridge address %s is already present on %s; clean up the existing tunnel before starting another", bridgeCIDR, existingBridge)
	}
	var command *exec.Cmd
	if cfg.useSudo {
		command = exec.Command("sudo", append([]string{"-n", cfg.VXLANBinary}, arguments...)...)
	} else {
		command = exec.Command(cfg.VXLANBinary, arguments...)
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start darwin-vxlan: %w", err)
	}
	log.Printf("started VXLAN child pid=%d podCIDR=%s bridgeCIDR=%s", command.Process.Pid, cidr, bridgeCIDR)
	wait := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(processDone)
	}()
	cleanup := func() {
		if command.Process == nil {
			return
		}
		signalProcessTree(command.Process.Pid, cfg.useSudo, syscall.SIGINT)
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			signalProcessTree(command.Process.Pid, cfg.useSudo, syscall.SIGKILL)
			select {
			case <-wait:
			case <-time.After(time.Second):
			}
		}
	}
	bridgeName, bridgeMAC, err := waitForBridge(ctx, bridgeCIDR, 30*time.Second, processDone)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("discover darwin-vxlan bridge: %w", err)
	}
	select {
	case <-processDone:
		cleanup()
		return nil, errors.New("darwin-vxlan exited after creating its bridge")
	default:
	}
	log.Printf("VXLAN bridge discovered name=%s mac=%s address=%s", bridgeName, bridgeMAC, bridgeCIDR)
	return &VXLANHandle{BridgeCIDR: bridgeCIDR, BridgeName: bridgeName, BridgeMAC: bridgeMAC, cleanup: cleanup}, nil
}

func signalProcessTree(pid int, useSudo bool, signal syscall.Signal) {
	if useSudo {
		if output, err := exec.Command("pgrep", "-P", fmt.Sprint(pid)).Output(); err == nil {
			for _, child := range strings.Fields(string(output)) {
				_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", signal), child).Run()
			}
		}
	}
	_ = syscall.Kill(pid, signal)
}

func interfaceForAddress(addressCIDR string) (string, error) {
	ip, _, err := net.ParseCIDR(addressCIDR)
	if err != nil {
		return "", fmt.Errorf("parse interface address %q: %w", addressCIDR, err)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var addressIP net.IP
			switch value := address.(type) {
			case *net.IPNet:
				addressIP = value.IP
			case *net.IPAddr:
				addressIP = value.IP
			}
			if addressIP != nil && addressIP.Equal(ip) {
				return iface.Name, nil
			}
		}
	}
	return "", nil
}

func waitForBridge(ctx context.Context, bridgeCIDR string, timeout time.Duration, processDone <-chan struct{}) (string, string, error) {
	ip, _, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return "", "", fmt.Errorf("parse bridge address %q: %w", bridgeCIDR, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-processDone:
			return "", "", errors.New("darwin-vxlan exited before its bridge became ready")
		default:
		}
		interfaces, listErr := net.Interfaces()
		if listErr == nil {
			for _, iface := range interfaces {
				if len(iface.HardwareAddr) == 0 {
					continue
				}
				addresses, addrErr := iface.Addrs()
				if addrErr != nil {
					continue
				}
				for _, address := range addresses {
					var addressIP net.IP
					switch value := address.(type) {
					case *net.IPNet:
						addressIP = value.IP
					case *net.IPAddr:
						addressIP = value.IP
					}
					if addressIP != nil && addressIP.Equal(ip) {
						return iface.Name, iface.HardwareAddr.String(), nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("bridge address %s did not appear", bridgeCIDR)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type flannelBackendData struct {
	VNI     int    `json:"VNI"`
	VtepMAC string `json:"VtepMAC"`
}

func flannelAnnotations(publicIP, bridgeMAC string) (map[string]string, error) {
	backendData, err := json.Marshal(flannelBackendData{VNI: 1, VtepMAC: bridgeMAC})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		flannelBackendTypeAnnotation:   "vxlan",
		flannelBackendDataAnnotation:   string(backendData),
		flannelPublicIPAnnotation:      publicIP,
		flannelSubnetManagerAnnotation: "true",
	}, nil
}

func configureFlannel(ctx context.Context, client *APIClient, node *Node, publicIP, bridgeMAC string) (*Node, error) {
	desired, err := flannelAnnotations(publicIP, bridgeMAC)
	if err != nil {
		return nil, err
	}
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		annotations := make(map[string]string, len(current.Metadata.Annotations)+len(desired))
		for key, value := range current.Metadata.Annotations {
			annotations[key] = value
		}
		changed := false
		for key, value := range desired {
			if annotations[key] != value {
				changed = true
			}
			annotations[key] = value
		}
		if !changed {
			return current, nil
		}
		body, patchErr := client.patchJSON(ctx, "/api/v1/nodes/"+url.PathEscape(current.Metadata.Name), map[string]any{
			"metadata": map[string]any{"annotations": annotations},
		})
		if patchErr == nil {
			var updated Node
			if err := json.Unmarshal(body, &updated); err != nil {
				return nil, fmt.Errorf("decode Flannel Node patch: %w", err)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(patchErr, &conflict) || conflict.Code != http.StatusConflict || attempt == 4 {
			return nil, fmt.Errorf("configure Flannel annotations: %w", patchErr)
		}
		latest, getErr := client.get(ctx, "/api/v1/nodes/"+url.PathEscape(current.Metadata.Name))
		if getErr != nil {
			return nil, fmt.Errorf("refresh Node after Flannel annotation conflict: %w", getErr)
		}
		var refreshed Node
		if getErr := json.Unmarshal(latest, &refreshed); getErr != nil {
			return nil, fmt.Errorf("decode Node after Flannel annotation conflict: %w", getErr)
		}
		current = &refreshed
	}
	return nil, errors.New("Flannel annotation retry limit exceeded")
}

func clearFlannel(ctx context.Context, client *APIClient, nodeName string) error {
	path := "/api/v1/nodes/" + url.PathEscape(nodeName)
	body, err := client.get(ctx, path)
	if err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return err
	}
	var node Node
	if err := json.Unmarshal(body, &node); err != nil {
		return err
	}
	remove := map[string]any{}
	for _, key := range []string{
		flannelBackendTypeAnnotation,
		flannelBackendDataAnnotation,
		flannelPublicIPAnnotation,
		flannelSubnetManagerAnnotation,
	} {
		if _, ok := node.Metadata.Annotations[key]; ok {
			remove[key] = nil
		}
	}
	if len(remove) == 0 {
		return nil
	}
	if _, err := client.patchJSON(ctx, path, map[string]any{"metadata": map[string]any{"annotations": remove}}); err != nil {
		return fmt.Errorf("clear Flannel annotations: %w", err)
	}
	return nil
}

func discoverFlannelPeers(ctx context.Context, client *APIClient, localNodeName string) ([]FlannelPeer, error) {
	body, err := client.get(ctx, "/api/v1/nodes")
	if err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
			return nil, errors.New("the peer client cannot read Nodes; provide a peer kubeconfig with Node list access or use --vxlan-gateway-mac for single-peer mode")
		}
		return nil, fmt.Errorf("list Nodes to discover Flannel peers: %w", err)
	}
	var nodes NodeList
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("decode Node list for Flannel peers: %w", err)
	}
	peers := make([]FlannelPeer, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Metadata.Name == localNodeName {
			continue
		}
		annotations := node.Metadata.Annotations
		if annotations[flannelBackendTypeAnnotation] != "vxlan" {
			continue
		}
		cidr := node.Spec.PodCIDR
		if cidr == "" && len(node.Spec.PodCIDRs) > 0 {
			cidr = node.Spec.PodCIDRs[0]
		}
		if cidr == "" || annotations[flannelPublicIPAnnotation] == "" || annotations[flannelBackendDataAnnotation] == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(cidr); err != nil || network.IP.To4() == nil {
			continue
		}
		var backend flannelBackendData
		if err := json.Unmarshal([]byte(annotations[flannelBackendDataAnnotation]), &backend); err != nil {
			return nil, fmt.Errorf("decode Flannel backend data for node %s: %w", node.Metadata.Name, err)
		}
		if backend.VNI != 1 {
			continue
		}
		mac, err := net.ParseMAC(backend.VtepMAC)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("node %s has invalid Flannel VtepMAC %q", node.Metadata.Name, backend.VtepMAC)
		}
		if net.ParseIP(annotations[flannelPublicIPAnnotation]) == nil {
			return nil, fmt.Errorf("node %s has invalid Flannel public IP %q", node.Metadata.Name, annotations[flannelPublicIPAnnotation])
		}
		peers = append(peers, FlannelPeer{
			NodeName: node.Metadata.Name,
			PodCIDR:  cidr,
			PublicIP: annotations[flannelPublicIPAnnotation],
			VtepMAC:  mac.String(),
		})
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].PodCIDR < peers[j].PodCIDR
	})
	return peers, nil
}

func remoteVtepMAC(ctx context.Context, client *APIClient, remoteIP, override string) (string, error) {
	if override != "" {
		mac, err := net.ParseMAC(override)
		if err != nil || len(mac) != 6 {
			return "", fmt.Errorf("invalid --vxlan-gateway-mac %q", override)
		}
		return mac.String(), nil
	}
	peers, err := discoverFlannelPeers(ctx, client, "")
	if err != nil {
		return "", err
	}
	for _, peer := range peers {
		if peer.PublicIP == remoteIP {
			return peer.VtepMAC, nil
		}
	}
	return "", fmt.Errorf("no Flannel node annotation matches VXLAN remote %q; use --vxlan-gateway-mac to provide that node's flannel.1 MAC", remoteIP)
}

func gatewayAddressForCIDR(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return "", fmt.Errorf("PodCIDR %q does not have a second usable IPv4 address", cidr)
	}
	value := binary.BigEndian.Uint32(ip4) + 2
	gateway := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(gateway, value)
	return gateway.String(), nil
}

func routeSpec(cidr string) (DarwinRoute, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return DarwinRoute{}, fmt.Errorf("parse network CIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return DarwinRoute{}, fmt.Errorf("network CIDR %q is not IPv4", cidr)
	}
	mask := network.Mask
	maskIP := net.IPv4(mask[0], mask[1], mask[2], mask[3]).String()
	return DarwinRoute{Network: ip4.String(), Netmask: maskIP}, nil
}

func workloadIPForOffset(cidr string, offset uint32) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	hostBits := bits - prefix
	if hostBits < 2 || hostBits > 16 {
		return "", fmt.Errorf("PodCIDR %q is outside the supported workload address range", cidr)
	}
	broadcastOffset := uint32(1<<hostBits) - 1
	// Offset 0 is the network address, offsets 1 and 2 are reserved for the
	// maclet bridge and synthetic Flannel gateway, and the final offset is the
	// broadcast address.
	if offset < 3 || offset >= broadcastOffset {
		return "", fmt.Errorf("workload address offset %d is unavailable in PodCIDR %q", offset, cidr)
	}
	value := binary.BigEndian.Uint32(ip4) + offset
	workloadIP := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(workloadIP, value)
	return workloadIP.String(), nil
}

func firstAvailableWorkloadIP(cidr string, used map[string]bool) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	hostBits := bits - prefix
	if hostBits < 2 || hostBits > 16 {
		return "", fmt.Errorf("PodCIDR %q is outside the supported workload address range", cidr)
	}
	broadcastOffset := uint32(1<<hostBits) - 1
	for offset := uint32(3); offset < broadcastOffset; offset++ {
		ip, err := workloadIPForOffset(cidr, offset)
		if err != nil {
			return "", err
		}
		if !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("PodCIDR %q has no available workload addresses", cidr)
}

func peerGatewayAddressForCIDR(cidr string, index int) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return "", fmt.Errorf("PodCIDR %q does not have enough gateway addresses", cidr)
	}
	hostBits := bits - prefix
	broadcastOffset := uint32(1<<hostBits) - 1
	var offset uint32
	if index == 0 {
		offset = 2
	} else {
		if uint32(index) >= broadcastOffset-2 {
			return "", fmt.Errorf("PodCIDR %q has no address for peer gateway %d", cidr, index)
		}
		offset = broadcastOffset - uint32(index)
	}
	value := binary.BigEndian.Uint32(ip4) + offset
	gateway := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(gateway, value)
	return gateway.String(), nil
}

func setupDarwinNetwork(cfg JoinConfig, vxlan *VXLANHandle, peers []FlannelPeer, gatewayMAC string) (*DarwinNetworkHandle, error) {
	gateway, err := peerGatewayAddressForCIDR(vxlan.BridgeCIDR, 0)
	if err != nil {
		return nil, err
	}
	mac, err := net.ParseMAC(gatewayMAC)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("parse Flannel gateway MAC %q", gatewayMAC)
	}
	orderedPeers := make([]FlannelPeer, 0, len(peers))
	for _, peer := range peers {
		if peer.PublicIP == cfg.VXLANRemote {
			orderedPeers = append(orderedPeers, peer)
		}
	}
	for _, peer := range peers {
		if peer.PublicIP != cfg.VXLANRemote {
			orderedPeers = append(orderedPeers, peer)
		}
	}
	if len(orderedPeers) > 0 && orderedPeers[0].PublicIP != cfg.VXLANRemote {
		return nil, fmt.Errorf("no discovered Flannel peer matches VXLAN remote %q", cfg.VXLANRemote)
	}
	handle := &DarwinNetworkHandle{
		Interface:  vxlan.BridgeName,
		PodCIDR:    vxlan.BridgeCIDR,
		Gateway:    gateway,
		GatewayMAC: mac.String(),
		useSudo:    cfg.useSudo,
	}
	addGateway := func(peer FlannelPeer, peerGateway, peerMAC string) error {
		for _, existing := range handle.PeerGateways {
			if existing.Gateway == peerGateway || existing.PodCIDR == peer.PodCIDR {
				return fmt.Errorf("duplicate Darwin peer gateway %s or PodCIDR %s", peerGateway, peer.PodCIDR)
			}
		}
		handle.PeerGateways = append(handle.PeerGateways, DarwinPeerGateway{
			PodCIDR: peer.PodCIDR, Gateway: peerGateway, MAC: peerMAC, PublicIP: peer.PublicIP,
		})
		handle.ARPs = append(handle.ARPs, DarwinARPEntry{IP: peerGateway, MAC: peerMAC})
		return nil
	}
	if len(orderedPeers) == 0 {
		if err := addGateway(FlannelPeer{PublicIP: cfg.VXLANRemote}, gateway, mac.String()); err != nil {
			return nil, err
		}
	} else {
		for index, peer := range orderedPeers {
			peerMAC, parseErr := net.ParseMAC(peer.VtepMAC)
			if parseErr != nil || len(peerMAC) != 6 {
				return nil, fmt.Errorf("parse Flannel VtepMAC %q for node %s", peer.VtepMAC, peer.NodeName)
			}
			peerGateway, gatewayErr := peerGatewayAddressForCIDR(vxlan.BridgeCIDR, index)
			if gatewayErr != nil {
				return nil, gatewayErr
			}
			if err := addGateway(peer, peerGateway, peerMAC.String()); err != nil {
				return nil, err
			}
		}
	}
	routes := make([]DarwinRoute, 0, 2+len(orderedPeers))
	for _, cidr := range []string{cfg.ClusterCIDR, cfg.ServiceCIDR} {
		route, err := routeSpec(cidr)
		if err != nil {
			return nil, err
		}
		route.Gateway = gateway
		routes = append(routes, route)
	}
	for _, peer := range orderedPeers {
		route, err := routeSpec(peer.PodCIDR)
		if err != nil {
			return nil, err
		}
		for _, peerGateway := range handle.PeerGateways {
			if peerGateway.PodCIDR == peer.PodCIDR {
				route.Gateway = peerGateway.Gateway
				break
			}
		}
		routes = append(routes, route)
	}
	for _, arp := range handle.ARPs {
		command := privilegedCommand(cfg.useSudo, "arp", "-S", arp.IP, arp.MAC, "ifscope", vxlan.BridgeName)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErr := handle.cleanup()
			if cleanupErr != nil {
				return nil, fmt.Errorf("install Darwin ARP gateway %s on %s: %w (cleanup: %v; output: %s)", arp.IP, vxlan.BridgeName, err, cleanupErr, strings.TrimSpace(string(output)))
			}
			return nil, fmt.Errorf("install Darwin ARP gateway %s on %s: %w (%s)", arp.IP, vxlan.BridgeName, err, strings.TrimSpace(string(output)))
		}
	}
	for _, route := range routes {
		command := privilegedCommand(cfg.useSudo, "route", "-n", "add", "-net", route.Network, "-netmask", route.Netmask, route.Gateway)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErr := handle.cleanup()
			if cleanupErr != nil {
				return nil, fmt.Errorf("add Darwin route %s/%s via %s: %w (cleanup: %v; output: %s)", route.Network, route.Netmask, route.Gateway, err, cleanupErr, strings.TrimSpace(string(output)))
			}
			return nil, fmt.Errorf("add Darwin route %s/%s via %s: %w (%s)", route.Network, route.Netmask, route.Gateway, err, strings.TrimSpace(string(output)))
		}
		handle.Routes = append(handle.Routes, route)
	}
	return handle, nil
}

func (h *DarwinNetworkHandle) addWorkloadIP(ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("workload address %q is not an IPv4 address", ip)
	}
	parsed = parsed.To4()
	_, network, err := net.ParseCIDR(h.PodCIDR)
	if err != nil {
		return fmt.Errorf("parse PodCIDR %q: %w", h.PodCIDR, err)
	}
	if !network.Contains(parsed) {
		return fmt.Errorf("workload address %s is outside PodCIDR %s", ip, h.PodCIDR)
	}
	if h.isReservedWorkloadIP(parsed, network.IP) {
		return fmt.Errorf("workload address %s is reserved by maclet", ip)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return fmt.Errorf("PodCIDR %q does not have usable workload addresses", h.PodCIDR)
	}
	hostBits := bits - prefix
	broadcast := binary.BigEndian.Uint32(network.IP.To4()) + uint32(1<<hostBits) - 1
	if binary.BigEndian.Uint32(parsed) == broadcast {
		return fmt.Errorf("workload address %s is the PodCIDR broadcast address", ip)
	}
	canonicalIP := parsed.String()
	for _, existing := range h.Aliases {
		if existing == canonicalIP {
			return nil
		}
	}
	command := privilegedCommand(h.useSudo, "ifconfig", h.Interface, "inet", canonicalIP, "netmask", "255.255.255.255", "alias")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("add workload address %s to %s: %w (%s)", canonicalIP, h.Interface, err, strings.TrimSpace(string(output)))
	}
	h.Aliases = append(h.Aliases, canonicalIP)
	return nil
}

func (h *DarwinNetworkHandle) isReservedWorkloadIP(ip, networkIP net.IP) bool {
	if ip.Equal(networkIP) {
		return true
	}
	bridgeIP, _ := bridgeAddressForCIDR(h.PodCIDR)
	bridgeAddress := net.ParseIP(strings.Split(bridgeIP, "/")[0])
	if ip.Equal(bridgeAddress) || ip.Equal(net.ParseIP(h.Gateway)) {
		return true
	}
	for _, peer := range h.PeerGateways {
		if ip.Equal(net.ParseIP(peer.Gateway)) {
			return true
		}
	}
	return false
}

func (h *DarwinNetworkHandle) firstAvailableWorkloadIP(used map[string]bool) (string, error) {
	_, network, err := net.ParseCIDR(h.PodCIDR)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", h.PodCIDR, err)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	broadcastOffset := uint32(1<<(bits-prefix)) - 1
	for offset := uint32(3); offset < broadcastOffset; offset++ {
		ip, err := workloadIPForOffset(h.PodCIDR, offset)
		if err != nil {
			return "", err
		}
		if !used[ip] && !h.isReservedWorkloadIP(net.ParseIP(ip), network.IP) {
			return ip, nil
		}
	}
	return "", fmt.Errorf("PodCIDR %q has no available workload addresses", h.PodCIDR)
}

func (h *DarwinNetworkHandle) removeWorkloadIP(ip string) error {
	canonicalIP := net.ParseIP(ip)
	if canonicalIP == nil || canonicalIP.To4() == nil {
		return fmt.Errorf("workload address %q is not an IPv4 address", ip)
	}
	canonicalIP = canonicalIP.To4()
	address := canonicalIP.String()
	index := -1
	for i, existing := range h.Aliases {
		if existing == address {
			index = i
			break
		}
	}
	if index == -1 {
		return nil
	}
	command := privilegedCommand(h.useSudo, "ifconfig", h.Interface, "inet", address, "-alias")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove workload address %s from %s: %w (%s)", address, h.Interface, err, strings.TrimSpace(string(output)))
	}
	h.Aliases = append(h.Aliases[:index], h.Aliases[index+1:]...)
	return nil
}

func (h *DarwinNetworkHandle) setGatewayMAC(mac string) error {
	parsed, err := net.ParseMAC(mac)
	if err != nil || len(parsed) != 6 {
		return fmt.Errorf("parse Flannel gateway MAC %q", mac)
	}
	mac = parsed.String()
	if h.GatewayMAC == mac {
		return nil
	}
	command := privilegedCommand(h.useSudo, "arp", "-S", h.Gateway, mac, "ifscope", h.Interface)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("update Darwin ARP gateway %s on %s: %w (%s)", h.Gateway, h.Interface, err, strings.TrimSpace(string(output)))
	}
	for index := range h.ARPs {
		if h.ARPs[index].IP == h.Gateway {
			h.ARPs[index].MAC = mac
			break
		}
	}
	h.GatewayMAC = mac
	return nil
}

func (h *DarwinNetworkHandle) cleanup() error {
	var cleanupErrors []error
	for i := len(h.Aliases) - 1; i >= 0; i-- {
		if err := h.removeWorkloadIP(h.Aliases[i]); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	for i := len(h.Routes) - 1; i >= 0; i-- {
		route := h.Routes[i]
		command := privilegedCommand(h.useSudo, "route", "-n", "delete", "-net", route.Network, "-netmask", route.Netmask, route.Gateway)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete route %s/%s: %w (%s)", route.Network, route.Netmask, err, strings.TrimSpace(string(output))))
		}
	}
	for i := len(h.ARPs) - 1; i >= 0; i-- {
		arp := h.ARPs[i]
		command := privilegedCommand(h.useSudo, "arp", "-d", arp.IP, "ifscope", h.Interface)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete ARP gateway %s: %w (%s)", arp.IP, err, strings.TrimSpace(string(output))))
		}
	}
	return errors.Join(cleanupErrors...)
}

func bridgeAddressForCIDR(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	if ip4 := network.IP.To4(); ip4 != nil {
		value := binary.BigEndian.Uint32(ip4)
		if value == ^uint32(0) {
			return "", errors.New("PodCIDR has no usable first host address")
		}
		value++
		first := make(net.IP, net.IPv4len)
		binary.BigEndian.PutUint32(first, value)
		prefix, _ := network.Mask.Size()
		return fmt.Sprintf("%s/%d", first.String(), prefix), nil
	}
	return "", errors.New("only IPv4 PodCIDRs are currently supported by the Darwin VXLAN bridge")
}

func runJoin(cfg JoinConfig) error {
	if err := preparePrivileges(&cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	state, client, err := bootstrap(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.NodeName == "" {
		cfg.NodeName = state.NodeName
	}
	if cfg.NodeIP == "" {
		cfg.NodeIP = state.NodeIP
	}
	if cfg.ExternalIP == "" {
		cfg.ExternalIP = state.ExternalIP
		if cfg.ExternalIP == "" {
			cfg.ExternalIP = vxlanPublicIP(cfg, state)
		}
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = defaultDrainTimeout
	}
	if cfg.PeerKubeconfig == "" {
		cfg.PeerKubeconfig = state.PeerKubeconfig
	}
	if cfg.PeerContext == "" {
		cfg.PeerContext = state.PeerContext
	}
	node, err := ensureNode(ctx, client, state.NodeName, state.NodeIP)
	if err != nil {
		return err
	}
	// Only remove a cordon that maclet itself installed during a previous
	// graceful shutdown. Preserve an operator's independent cordon.
	if node.Metadata.Annotations[shutdownCordonAnnotation] == "true" {
		node, err = setNodeShutdownCordon(ctx, client, node, false)
		if err != nil {
			return err
		}
	}
	log.Printf("Node %s registered with labels kubernetes.io/os=darwin kubernetes.io/arch=arm64 and taint %s=%s:NoSchedule", state.NodeName, managedTaintKey, managedTaintValue)
	node, err = waitForPodCIDR(ctx, client, node, 60*time.Second)
	if err != nil {
		return fmt.Errorf("wait for PodCIDR: %w", err)
	}
	if node.Spec.PodCIDR != "" {
		log.Printf("Kubernetes assigned PodCIDR %s", node.Spec.PodCIDR)
	} else {
		log.Printf("Kubernetes has not assigned a PodCIDR yet; continuing without starting VXLAN")
	}
	var peerClient *APIClient
	var peers []FlannelPeer
	var gatewayMAC string
	if cfg.VXLANBinary != "" && cfg.VXLANGatewayMAC == "" {
		peerClient, err = peerAPIClient(cfg, state)
		if err != nil {
			return err
		}
		peers, err = discoverFlannelPeers(ctx, peerClient, state.NodeName)
		if err != nil {
			return err
		}
		for _, peer := range peers {
			if peer.PublicIP == cfg.VXLANRemote {
				gatewayMAC = peer.VtepMAC
				break
			}
		}
		if gatewayMAC == "" {
			return fmt.Errorf("no Flannel peer matches VXLAN remote %q; use --vxlan-gateway-mac to provide that node's flannel.1 MAC", cfg.VXLANRemote)
		}
	}
	vxlan, err := startVXLAN(ctx, cfg, node, peers)
	if err != nil {
		return err
	}
	var darwinNetwork *DarwinNetworkHandle
	var workloads *workloadManager
	if vxlan != nil {
		if gatewayMAC == "" {
			gatewayMAC, err = remoteVtepMAC(ctx, client, cfg.VXLANRemote, cfg.VXLANGatewayMAC)
			if err != nil {
				vxlan.cleanup()
				return err
			}
		}
		darwinNetwork, err = setupDarwinNetwork(cfg, vxlan, peers, gatewayMAC)
		if err != nil {
			vxlan.cleanup()
			return err
		}
		node, err = configureFlannel(ctx, client, node, vxlanPublicIP(cfg, state), vxlan.BridgeMAC)
		if err != nil {
			if cleanupErr := darwinNetwork.cleanup(); cleanupErr != nil {
				log.Printf("warning: clean Darwin routes after Flannel setup failure: %v", cleanupErr)
			}
			vxlan.cleanup()
			return err
		}
		log.Printf("published Flannel VXLAN metadata for %s: publicIP=%s vtepMAC=%s gatewayMAC=%s", state.NodeName, vxlanPublicIP(cfg, state), vxlan.BridgeMAC, gatewayMAC)
		workloads = newWorkloadManagerWithState(darwinNetwork, cfg.MackerBinary, state.NodeIP, cfg.StateDir)
		if err := workloads.loadJournal(); err != nil {
			darwinNetwork.cleanup()
			vxlan.cleanup()
			return fmt.Errorf("load native workload journal: %w", err)
		}
		defer func() {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if workloads != nil {
				if err := workloads.cleanup(); err != nil {
					log.Printf("warning: clean native workloads: %v", err)
				}
			}
			if err := clearFlannel(cleanupContext, client, state.NodeName); err != nil {
				log.Printf("warning: clear Flannel annotations: %v", err)
			}
			if err := darwinNetwork.cleanup(); err != nil {
				log.Printf("warning: clean Darwin routes: %v", err)
			}
			vxlan.cleanup()
		}()
	}
	node, err = updateNodeStatus(ctx, client, node, state.NodeIP, cfg.ExternalIP)
	if err != nil {
		return err
	}
	if err := ensureLease(ctx, client, state.NodeName); err != nil {
		return err
	}
	if !cfg.Once && workloads != nil {
		if pods, listErr := listAssignedPods(ctx, client, state.NodeName); listErr != nil {
			log.Printf("warning: list assigned workloads: %v", listErr)
		} else if reconcileErr := workloads.reconcile(ctx, client, pods); reconcileErr != nil {
			log.Printf("warning: reconcile native workloads: %v", reconcileErr)
		}
	}

	if cfg.Once {
		return nil
	}
	ticker := time.NewTicker(defaultHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			drainContext, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
			if node != nil {
				if drainedNode, drainErr := setNodeShutdownCordon(drainContext, client, node, true); drainErr != nil {
					log.Printf("warning: cordon Node before shutdown: %v", drainErr)
				} else {
					node = drainedNode
				}
			}
			if workloads != nil {
				if cleanupErr := workloads.cleanup(); cleanupErr != nil {
					log.Printf("warning: drain native workloads: %v", cleanupErr)
				}
			}
			cancel()
			return nil
		case <-ticker.C:
			if darwinNetwork != nil && peerClient != nil {
				gatewayMAC, peerErr := remoteVtepMAC(ctx, peerClient, cfg.VXLANRemote, "")
				if peerErr != nil {
					log.Printf("warning: refresh Flannel gateway MAC: %v", peerErr)
				} else if updateErr := darwinNetwork.setGatewayMAC(gatewayMAC); updateErr != nil {
					log.Printf("warning: update Flannel gateway MAC: %v", updateErr)
				}
			}
			body, err := client.get(ctx, "/api/v1/nodes/"+url.PathEscape(state.NodeName))
			if err != nil {
				return fmt.Errorf("refresh Node: %w", err)
			}
			if err := json.Unmarshal(body, &node); err != nil {
				return err
			}
			node, err = updateNodeStatus(ctx, client, node, state.NodeIP, cfg.ExternalIP)
			if err != nil {
				return err
			}
			if err := ensureLease(ctx, client, state.NodeName); err != nil {
				return err
			}
			if workloads != nil {
				if pods, listErr := listAssignedPods(ctx, client, state.NodeName); listErr != nil {
					log.Printf("warning: list assigned workloads: %v", listErr)
				} else if reconcileErr := workloads.reconcile(ctx, client, pods); reconcileErr != nil {
					log.Printf("warning: reconcile native workloads: %v", reconcileErr)
				}
			}
		}
	}
}

type Workload struct {
	Namespace      string   `json:"namespace"`
	Name           string   `json:"name"`
	UID            string   `json:"uid,omitempty"`
	Phase          string   `json:"phase,omitempty"`
	PodIP          string   `json:"podIP,omitempty"`
	HostIP         string   `json:"hostIP,omitempty"`
	Containers     []string `json:"containers,omitempty"`
	InitContainers []string `json:"initContainers,omitempty"`
}

type WorkloadSnapshot struct {
	Node        string     `json:"node"`
	GeneratedAt time.Time  `json:"generatedAt"`
	Workloads   []Workload `json:"workloads"`
}

func workloadSnapshot(ctx context.Context, client *APIClient, nodeName string) (WorkloadSnapshot, error) {
	query := url.Values{"fieldSelector": []string{"spec.nodeName=" + nodeName}}
	body, err := client.get(ctx, "/api/v1/pods?"+query.Encode())
	if err != nil {
		return WorkloadSnapshot{}, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return WorkloadSnapshot{}, fmt.Errorf("decode PodList: %w", err)
	}
	snapshot := WorkloadSnapshot{Node: nodeName, GeneratedAt: time.Now().UTC(), Workloads: make([]Workload, 0, len(pods.Items))}
	for _, pod := range pods.Items {
		workload := Workload{Namespace: pod.Metadata.Namespace, Name: pod.Metadata.Name, UID: pod.Metadata.UID, Phase: pod.Status.Phase, PodIP: pod.Status.PodIP, HostIP: pod.Status.HostIP}
		for _, container := range pod.Spec.Containers {
			workload.Containers = append(workload.Containers, container.Name+"="+container.Image)
		}
		for _, container := range pod.Spec.InitContainers {
			workload.InitContainers = append(workload.InitContainers, container.Name+"="+container.Image)
		}
		snapshot.Workloads = append(snapshot.Workloads, workload)
	}
	return snapshot, nil
}

func runWorkloads(stateDir string, insecure bool) error {
	body, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return fmt.Errorf("read maclet state: %w (run maclet join first)", err)
	}
	var state LocalState
	if err := json.Unmarshal(body, &state); err != nil {
		return err
	}
	client, err := newAPIClient(state.Server, mustReadFile(state.CAFile), state.ClientCert, state.ClientKey, insecure, "", "")
	if err != nil {
		return err
	}
	snapshot, err := workloadSnapshot(context.Background(), client, state.NodeName)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}

func defaultStatePath() string {
	home := homeDirectory()
	if home == "" {
		return defaultStateDir
	}
	return filepath.Join(home, defaultStateDir)
}

func usage() {
	fmt.Fprintf(os.Stderr, `maclet %s

Usage:
  maclet join [options]       register/heartbeat a Darwin node
  maclet workloads [options]  print pods scheduled to this node as JSON
  maclet version

Run "maclet join --help" or "maclet workloads --help" for command options.
`, version)
}

func runJoinCommand(args []string) error {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cfg := JoinConfig{}
	flags.StringVar(&cfg.Server, "server", "", "Kubernetes/K3s API URL (https://host:6443)")
	flags.StringVar(&cfg.Token, "token", "", "K3s join token (prefer --token-file to avoid process listings)")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "read K3s join token from this file, or - for stdin")
	flags.StringVar(&cfg.NodeName, "node-name", defaultNodeName, "Kubernetes node name")
	flags.StringVar(&cfg.NodeIP, "node-ip", "", "node/underlay IP advertised to Kubernetes (auto-detected if empty)")
	flags.StringVar(&cfg.ExternalIP, "external-ip", "", "Kubernetes ExternalIP address (defaults to --vxlan-local or --node-ip)")
	flags.StringVar(&cfg.StateDir, "state-dir", defaultStatePath(), "maclet state directory")
	flags.BoolVar(&cfg.InsecureSkipTLSVerify, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	flags.BoolVar(&cfg.Once, "once", false, "register, heartbeat once, and exit")
	flags.StringVar(&cfg.VXLANBinary, "vxlan-binary", "", "path to darwin-vxlan; start it after PodCIDR assignment")
	flags.StringVar(&cfg.MackerBinary, "macker-binary", "", "path to Macker; start assigned native Pods through it (defaults to PATH)")
	flags.StringVar(&cfg.VXLANRemote, "vxlan-remote", "", "VXLAN remote underlay address")
	flags.StringVar(&cfg.VXLANLocal, "vxlan-local", "", "VXLAN local underlay address (defaults to --node-ip)")
	flags.StringVar(&cfg.VXLANGatewayMAC, "vxlan-gateway-mac", "", "static remote flannel.1 MAC override (normally discovered through --peer-kubeconfig)")
	flags.StringVar(&cfg.PeerKubeconfig, "peer-kubeconfig", "", "kubeconfig used to discover peer Flannel Node annotations (defaults to $KUBECONFIG or ~/.kube/config)")
	flags.StringVar(&cfg.PeerContext, "peer-context", "", "kubeconfig context used for peer discovery (defaults to the current context)")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", defaultVXLANPort, "VXLAN UDP port")
	flags.IntVar(&cfg.VXLANMTU, "vxlan-mtu", defaultVXLANMTU, "VXLAN bridge MTU")
	flags.StringVar(&cfg.ClusterCIDR, "cluster-cidr", defaultClusterCIDR, "cluster Pod network CIDR routed through the Darwin VXLAN")
	flags.StringVar(&cfg.ServiceCIDR, "service-cidr", defaultServiceCIDR, "Kubernetes Service network CIDR routed through the Darwin VXLAN")
	flags.DurationVar(&cfg.DrainTimeout, "drain-timeout", defaultDrainTimeout, "maximum time for API cordon during graceful shutdown")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runJoin(cfg)
}

func runWorkloadsCommand(args []string) error {
	flags := flag.NewFlagSet("workloads", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := defaultStatePath()
	insecure := false
	flags.StringVar(&stateDir, "state-dir", stateDir, "maclet state directory")
	flags.BoolVar(&insecure, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return runWorkloads(stateDir, insecure)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "join":
		err = runJoinCommand(os.Args[2:])
	case "workloads":
		err = runWorkloadsCommand(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}
