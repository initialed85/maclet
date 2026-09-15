package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// workloadAPIClient is the separately authorized API identity used for
// namespace-scoped ConfigMaps and Secrets. The system:node client is
// intentionally not broadened to read arbitrary configuration objects.
func (m *workloadManager) workloadAPIClient() *APIClient {
	return m.apiClient
}

func (m *workloadManager) resolveContainerEnvironment(ctx context.Context, pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	values := make(map[string]string)
	for index, source := range container.EnvFrom {
		if source.ConfigMapRef != nil && source.SecretRef != nil {
			return nil, fmt.Errorf("envFrom[%d] specifies both ConfigMap and Secret", index)
		}
		switch {
		case source.ConfigMapRef != nil:
			configMap, found, err := m.getConfigMap(ctx, pod.ObjectMeta.Namespace, source.ConfigMapRef.Name, source.ConfigMapRef.Optional != nil && *source.ConfigMapRef.Optional)
			if err != nil {
				return nil, fmt.Errorf("envFrom ConfigMap %q: %w", source.ConfigMapRef.Name, err)
			}
			if !found {
				continue
			}
			keys := make([]string, 0, len(configMap.Data))
			for key := range configMap.Data {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				values[source.Prefix+key] = configMap.Data[key]
			}
		case source.SecretRef != nil:
			secret, found, err := m.getSecret(ctx, pod.ObjectMeta.Namespace, source.SecretRef.Name, source.SecretRef.Optional != nil && *source.SecretRef.Optional)
			if err != nil {
				return nil, fmt.Errorf("envFrom Secret %q: %w", source.SecretRef.Name, err)
			}
			if !found {
				continue
			}
			keys := make([]string, 0, len(secret.Data))
			for key := range secret.Data {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				values[source.Prefix+key] = string(secret.Data[key])
			}
		default:
			return nil, fmt.Errorf("envFrom[%d] must specify a ConfigMapRef or SecretRef", index)
		}
	}

	for index, env := range container.Env {
		if env.Name == "" {
			return nil, fmt.Errorf("container environment variable %d has an empty name", index)
		}
		value := env.Value
		if env.ValueFrom != nil {
			if env.ValueFrom.FieldRef == nil {
				return nil, fmt.Errorf("environment variable %q uses an unsupported valueFrom source", env.Name)
			}
			resolved, err := downwardAPIValue(pod, managed, m.nodeIP, env.ValueFrom.FieldRef.FieldPath)
			if err != nil {
				return nil, fmt.Errorf("environment variable %q: %w", env.Name, err)
			}
			value = resolved
		}
		values[env.Name] = value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

func (m *workloadManager) getConfigMap(ctx context.Context, namespace, name string, optional bool) (ConfigMap, bool, error) {
	if name == "" {
		return ConfigMap{}, false, errors.New("ConfigMap name is empty")
	}
	client := m.workloadAPIClient()
	if client == nil {
		return ConfigMap{}, false, missingPeerStorageClientError("ConfigMap")
	}
	body, err := client.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/configmaps/"+url.PathEscape(name))
	if err != nil {
		var apiErr *HTTPError
		if optional && errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return ConfigMap{}, false, nil
		}
		return ConfigMap{}, false, authorizedPeerStorageError("ConfigMap "+namespace+"/"+name, err)
	}
	var configMap ConfigMap
	if err := json.Unmarshal(body, &configMap); err != nil {
		return ConfigMap{}, false, fmt.Errorf("decode ConfigMap: %w", err)
	}
	return configMap, true, nil
}

func (m *workloadManager) getSecret(ctx context.Context, namespace, name string, optional bool) (Secret, bool, error) {
	if name == "" {
		return Secret{}, false, errors.New("Secret name is empty")
	}
	client := m.workloadAPIClient()
	if client == nil {
		return Secret{}, false, missingPeerStorageClientError("Secret")
	}
	body, err := client.Get(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name))
	if err != nil {
		var apiErr *HTTPError
		if optional && errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return Secret{}, false, nil
		}
		return Secret{}, false, authorizedPeerStorageError("Secret "+namespace+"/"+name, err)
	}
	var secret Secret
	if err := json.Unmarshal(body, &secret); err != nil {
		return Secret{}, false, fmt.Errorf("decode Secret: %w", err)
	}
	return secret, true, nil
}

func downwardAPIValue(pod Pod, managed *managedWorkload, hostIP, fieldPath string) (string, error) {
	fieldPath = strings.TrimSpace(fieldPath)
	switch fieldPath {
	case "metadata.name":
		return pod.ObjectMeta.Name, nil
	case "metadata.namespace":
		return pod.ObjectMeta.Namespace, nil
	case "metadata.uid":
		return string(pod.ObjectMeta.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.podIP":
		if pod.Status.PodIP != "" {
			return pod.Status.PodIP, nil
		}
		if managed != nil && managed.IP != "" {
			return managed.IP, nil
		}
		return "", nil
	case "status.hostIP":
		if pod.Status.HostIP != "" {
			return pod.Status.HostIP, nil
		}
		return hostIP, nil
	}
	if key, ok := metadataFieldKey(fieldPath, "labels"); ok {
		return pod.ObjectMeta.Labels[key], nil
	}
	if key, ok := metadataFieldKey(fieldPath, "annotations"); ok {
		return pod.ObjectMeta.Annotations[key], nil
	}
	return "", fmt.Errorf("fieldRef %q is not supported", fieldPath)
}

func metadataFieldKey(fieldPath, field string) (string, bool) {
	prefix := "metadata." + field + "['"
	if !strings.HasPrefix(fieldPath, prefix) || !strings.HasSuffix(fieldPath, "']") {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(fieldPath, prefix), "']")
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", false
	}
	return key, true
}

func redactMackerArgs(args []string) string {
	redacted := append([]string(nil), args...)
	for index := 0; index+1 < len(redacted); index++ {
		if redacted[index] == "--env" {
			if equals := strings.IndexByte(redacted[index+1], '='); equals >= 0 {
				redacted[index+1] = redacted[index+1][:equals+1] + "<redacted>"
			}
		}
	}
	return formatCommandArgs(redacted)
}
