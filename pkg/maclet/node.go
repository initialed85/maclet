package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: ObjectMeta{
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
	alreadyMarked := node.ObjectMeta.Annotations[shutdownCordonAnnotation] == "true"
	if cordon && node.Spec.Unschedulable && alreadyMarked {
		return node, nil
	}
	if !cordon && !alreadyMarked {
		return node, nil
	}
	path := "/api/v1/nodes/" + url.PathEscape(node.ObjectMeta.Name)
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		annotations := map[string]any{shutdownCordonAnnotation: marker}
		if !cordon {
			annotations[shutdownCordonAnnotation] = nil
		}
		body, err := client.PatchJSON(ctx, path, map[string]any{
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
			return nil, fmt.Errorf("set Node %q shutdown cordon=%t: %w", current.ObjectMeta.Name, cordon, err)
		}
		body, err = client.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("refresh Node after shutdown cordon conflict: %w", err)
		}
		var refreshed Node
		if err := json.Unmarshal(body, &refreshed); err != nil {
			return nil, fmt.Errorf("decode Node after shutdown cordon conflict: %w", err)
		}
		current = &refreshed
		alreadyMarked = current.ObjectMeta.Annotations[shutdownCordonAnnotation] == "true"
		if (cordon && current.Spec.Unschedulable && alreadyMarked) || (!cordon && !alreadyMarked) {
			return current, nil
		}
	}
	return nil, errors.New("Node shutdown cordon update retry limit exceeded")
}

func ensureNode(ctx context.Context, client *APIClient, name, nodeIP string) (*Node, error) {
	path := "/api/v1/nodes/" + url.PathEscape(name)
	body, err := client.Get(ctx, path)
	if err != nil {
		var apiErr *HTTPError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
			return nil, err
		}
		desired := desiredNode(name, nodeIP)
		body, err = client.PostJSON(ctx, "/api/v1/nodes", desired)
		if err != nil {
			var conflict *HTTPError
			if errors.As(err, &conflict) && conflict.Code == http.StatusConflict {
				body, err = client.Get(ctx, path)
			} else {
				return nil, fmt.Errorf("create Node %q: %w", name, err)
			}
		}
	}
	var node Node
	if err := json.Unmarshal(body, &node); err != nil {
		return nil, fmt.Errorf("decode Node %q: %w", name, err)
	}

	labels := node.ObjectMeta.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	patchLabels := map[string]string{}
	for key, value := range desiredNode(name, nodeIP).ObjectMeta.Labels {
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
		for key, value := range desiredNode(name, nodeIP).ObjectMeta.Labels {
			mergedLabels[key] = value
		}
		patch := map[string]any{
			"metadata": map[string]any{
				"labels": mergedLabels,
			},
			"spec": map[string]any{"taints": mergedTaints},
		}
		body, err = client.PatchJSON(ctx, path, patch)
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
	stamp := metav1.NewTime(now.UTC())
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
	podCapacity := *resource.NewQuantity(defaultMaxPods, resource.DecimalSI)
	cpuCapacity := *resource.NewMilliQuantity(int64(runtime.NumCPU())*1000, resource.DecimalSI)
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:  cpuCapacity,
		corev1.ResourcePods: podCapacity,
	}
	allocatable := corev1.ResourceList{
		corev1.ResourceCPU:  cpuCapacity,
		corev1.ResourcePods: podCapacity,
	}
	if memoryCapacity, err := hostMemoryCapacityBytes(); err == nil && memoryCapacity <= uint64(^uint64(0)>>1) {
		memoryQuantity := *resource.NewQuantity(int64(memoryCapacity), resource.BinarySI)
		capacity[corev1.ResourceMemory] = memoryQuantity
		allocatable[corev1.ResourceMemory] = memoryQuantity
	}
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
		DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: defaultKubeletPort}},
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

func nodeStatusPayload(current *Node, status NodeStatus, includeSpec bool) map[string]any {
	metadata := map[string]any{
		"name":            current.ObjectMeta.Name,
		"resourceVersion": current.ObjectMeta.ResourceVersion,
		"labels":          current.ObjectMeta.Labels,
		"annotations":     current.ObjectMeta.Annotations,
	}
	if current.ObjectMeta.UID != "" {
		metadata["uid"] = current.ObjectMeta.UID
	}
	payload := map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   metadata,
		"status":     status,
	}
	if includeSpec {
		// Preserve the current spec on the normal path. Some K3s/API-server
		// combinations run NodeRestriction against the submitted object before
		// the status strategy restores the persisted spec.
		payload["spec"] = current.Spec
	}
	return payload
}

func isNodeTaintRestrictionError(err error) bool {
	var apiErr *HTTPError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden && strings.Contains(strings.ToLower(apiErr.Body), "not allowed to modify taints")
}

func updateNodeStatus(ctx context.Context, client *APIClient, node *Node, nodeIP, externalIP string) (*Node, error) {
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		status := nodeStatus(current.ObjectMeta.Name, nodeIP, externalIP, time.Now())
		payload := nodeStatusPayload(current, status, true)
		body, err := client.PutJSON(ctx, "/api/v1/nodes/"+url.PathEscape(current.ObjectMeta.Name)+"/status", payload)
		if err == nil {
			var updated Node
			if err := json.Unmarshal(body, &updated); err != nil {
				return nil, fmt.Errorf("decode Node status response: %w", err)
			}
			return &updated, nil
		}
		if isNodeTaintRestrictionError(err) {
			// A stale full Node object can make NodeRestriction compare a changed
			// taint set even though this is a status-only operation. Refresh the
			// object, then retry with its complete authoritative spec. Omitting
			// spec is not safe on K3s versions whose admission path compares the
			// submitted object before the status strategy restores persisted fields.
			latest, getErr := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(current.ObjectMeta.Name))
			if getErr != nil {
				return nil, fmt.Errorf("refresh Node after taint restriction: %w", getErr)
			}
			var refreshed Node
			if getErr := json.Unmarshal(latest, &refreshed); getErr != nil {
				return nil, fmt.Errorf("decode Node after taint restriction: %w", getErr)
			}
			current = &refreshed
			continue
		}
		var conflict *HTTPError
		if !errors.As(err, &conflict) || conflict.Code != http.StatusConflict || attempt == 4 {
			return nil, fmt.Errorf("update Node %q status: %w", current.ObjectMeta.Name, err)
		}
		latest, getErr := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(current.ObjectMeta.Name))
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
	now := metav1.MicroTime{Time: time.Now().UTC()}
	holderIdentity := nodeName
	leaseDuration := int32(defaultLeaseDurationSecs)
	body, err := client.Get(ctx, path)
	lease := Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: ObjectMeta{Name: nodeName, Namespace: "kube-node-lease"},
		Spec: LeaseSpec{
			HolderIdentity:       &holderIdentity,
			LeaseDurationSeconds: &leaseDuration,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	if err != nil {
		var apiErr *HTTPError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
			return err
		}
		if _, err := client.PostJSON(ctx, "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases", lease); err != nil {
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
	lease.Spec.HolderIdentity = &holderIdentity
	lease.Spec.LeaseDurationSeconds = &leaseDuration
	lease.Spec.RenewTime = &now
	if _, err := client.PutJSON(ctx, path, lease); err != nil {
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
		body, err := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(node.ObjectMeta.Name))
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
