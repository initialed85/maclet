package maclet

import (
	"errors"
	"fmt"
	"net/http"
)

func authorizedPeerStorageError(resource string, err error) error {
	var apiErr *HTTPError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
		return fmt.Errorf("get %s: API authorization denied; configure --peer-kubeconfig with an identity authorized to read PVCs/PVs, ConfigMaps, and Secrets: %w", resource, err)
	}
	return err
}

func missingPeerStorageClientError(resource string) error {
	return fmt.Errorf("%s lookup requires an explicitly authorized peer API client; configure --peer-kubeconfig (the token-backed system:k3s-controller identity is intentionally not assumed to read storage/config objects)", resource)
}
