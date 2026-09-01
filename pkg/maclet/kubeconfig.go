package maclet

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

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

func inClusterAPIClient() (*APIClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster environment is unavailable")
	}
	caPEM, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read in-cluster CA: %w", err)
	}
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("read in-cluster token: %w", err)
	}
	client, err := newAPIClientWithMaterial("https://"+net.JoinHostPort(host, port), caPEM, nil, nil, false, "", "", strings.TrimSpace(string(token)))
	if err != nil {
		return nil, fmt.Errorf("create in-cluster API client: %w", err)
	}
	return client, nil
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
	yamlBody, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return nil, false, fmt.Errorf("read peer kubeconfig: %w", err)
	}
	body, err := yaml.YAMLToJSON(yamlBody)
	if err != nil {
		return nil, false, fmt.Errorf("decode peer kubeconfig YAML: %w", err)
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
	if server == "" {
		server = cluster.server
	}
	client, err := newAPIClientWithMaterial(server, caPEM, certPEM, keyPEM, insecure, selectedUser.username, selectedUser.password, token)
	if err != nil {
		return nil, false, fmt.Errorf("create peer API client: %w", err)
	}
	return client, true, nil
}
