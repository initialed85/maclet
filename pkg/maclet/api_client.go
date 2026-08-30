package maclet

import (
	"net/url"

	kubeapi "github.com/initialed85/maclet/pkg/kube"
)

// APIClient and HTTPError are retained as local aliases so the command code
// stays focused on maclet behavior while the transport implementation lives in
// pkg/kube.
type APIClient = kubeapi.Client
type HTTPError = kubeapi.HTTPError

func newAPIClient(server string, caPEM []byte, certFile, keyFile string, insecure bool, username, password string) (*APIClient, error) {
	return kubeapi.NewClient(server, caPEM, certFile, keyFile, insecure, username, password)
}

func newAPIClientWithMaterial(server string, caPEM, certPEM, keyPEM []byte, insecure bool, username, password, bearerToken string) (*APIClient, error) {
	return kubeapi.NewClientWithMaterial(server, caPEM, certPEM, keyPEM, insecure, username, password, bearerToken)
}

func normalizeServer(server string) (*url.URL, error) {
	return kubeapi.NormalizeServer(server)
}
