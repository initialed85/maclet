package maclet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const nodeInstanceAnnotation = "k8s-darwin.dev/maclet-instance-id"

var errNodeNameConflict = errors.New("Kubernetes Node name conflict")

func randomInstanceID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func ensureStateInstanceID(state *LocalState, statePath string) error {
	if state.InstanceID != "" {
		return nil
	}
	instanceID, err := randomInstanceID()
	if err != nil {
		return fmt.Errorf("generate maclet instance ID: %w", err)
	}
	state.InstanceID = instanceID
	if err := writeLocalState(statePath, state); err != nil {
		return fmt.Errorf("persist maclet instance ID: %w", err)
	}
	return nil
}

func desiredOwnedNode(name, nodeIP, instanceID string) Node {
	node := desiredNode(name, nodeIP)
	node.ObjectMeta.Annotations = map[string]string{nodeInstanceAnnotation: instanceID}
	return node
}

func nodeOwnershipConflict(name, reason string) error {
	return fmt.Errorf("%w: Node %q %s", errNodeNameConflict, name, reason)
}

func verifyNodeOwnership(node *Node, name, instanceID, expectedUID string, adopt bool) error {
	if node.ObjectMeta.Name != name {
		return nodeOwnershipConflict(name, "API returned a different Node")
	}
	marker, marked := node.ObjectMeta.Annotations[nodeInstanceAnnotation]
	if marked && marker != instanceID {
		return nodeOwnershipConflict(name, fmt.Sprintf("has maclet instance marker %q, expected %q", marker, instanceID))
	}
	if expectedUID != "" {
		if node.ObjectMeta.UID == "" {
			return nodeOwnershipConflict(name, "has no UID to verify persisted ownership")
		}
		if string(node.ObjectMeta.UID) != expectedUID {
			return nodeOwnershipConflict(name, fmt.Sprintf("has UID %q, expected persisted UID %q", node.ObjectMeta.UID, expectedUID))
		}
	}
	if !marked {
		if !adopt {
			return nodeOwnershipConflict(name, "is not marked as owned by this maclet instance; rerun once with --adopt to claim it")
		}
		if err := validateAdoptableNode(node, name); err != nil {
			return err
		}
	}
	return nil
}

func validateAdoptableNode(node *Node, name string) error {
	labels := node.ObjectMeta.Labels
	if value := labels["kubernetes.io/os"]; value != "" && value != "darwin" {
		return nodeOwnershipConflict(name, fmt.Sprintf("has incompatible kubernetes.io/os label %q", value))
	}
	if value := labels["kubernetes.io/arch"]; value != "" && value != "arm64" {
		return nodeOwnershipConflict(name, fmt.Sprintf("has incompatible kubernetes.io/arch label %q", value))
	}
	if value := labels[managedLabelKey]; value != "" && value != managedLabelValue {
		return nodeOwnershipConflict(name, fmt.Sprintf("has incompatible %s label %q", managedLabelKey, value))
	}
	return nil
}

func persistNodeOwnership(state *LocalState, statePath string, node *Node) error {
	if node.ObjectMeta.UID == "" {
		return nodeOwnershipConflict(state.NodeName, "API returned no UID")
	}
	if state.NodeUID == string(node.ObjectMeta.UID) {
		return nil
	}
	state.NodeUID = string(node.ObjectMeta.UID)
	if err := writeLocalState(statePath, state); err != nil {
		return fmt.Errorf("persist Node UID: %w", err)
	}
	return nil
}

func ensureOwnedNode(ctx context.Context, client *APIClient, name, nodeIP, instanceID, expectedUID string, adopt bool) (*Node, error) {
	if instanceID == "" {
		return nil, errors.New("maclet instance ID is required for Node ownership")
	}
	path := "/api/v1/nodes/" + url.PathEscape(name)
	created := false
	body, err := client.Get(ctx, path)
	if err != nil {
		var apiErr *HTTPError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
			return nil, err
		}
		desired := desiredOwnedNode(name, nodeIP, instanceID)
		body, err = client.PostJSON(ctx, "/api/v1/nodes", desired)
		if err != nil {
			var conflict *HTTPError
			if !errors.As(err, &conflict) || conflict.Code != http.StatusConflict {
				return nil, fmt.Errorf("create Node %q: %w", name, err)
			}
			// A concurrent create is never an invitation to overwrite or delete
			// the object. Fetch it and apply the same ownership checks below.
			body, err = client.Get(ctx, path)
			if err != nil {
				return nil, fmt.Errorf("refresh Node %q after create conflict: %w", name, err)
			}
		} else {
			created = true
		}
	}
	var node Node
	if err := json.Unmarshal(body, &node); err != nil {
		return nil, fmt.Errorf("decode Node %q: %w", name, err)
	}
	verifyUID := expectedUID
	if created {
		// A deleted owned Node receives a new Kubernetes UID when recreated.
		verifyUID = ""
	}
	if err := verifyNodeOwnership(&node, name, instanceID, verifyUID, adopt); err != nil {
		return nil, err
	}

	desired := desiredOwnedNode(name, nodeIP, instanceID)
	labels := node.ObjectMeta.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	mergedLabels := make(map[string]string, len(labels)+len(desired.ObjectMeta.Labels))
	for key, value := range labels {
		mergedLabels[key] = value
	}
	for key, value := range desired.ObjectMeta.Labels {
		mergedLabels[key] = value
	}
	mergedTaints := append([]Taint(nil), node.Spec.Taints...)
	if !hasManagedTaint(mergedTaints) {
		mergedTaints = append(mergedTaints, Taint{Key: managedTaintKey, Value: managedTaintValue, Effect: managedTaintEffect})
	}
	marker := node.ObjectMeta.Annotations[nodeInstanceAnnotation]
	annotations := node.ObjectMeta.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	mergedAnnotations := make(map[string]string, len(annotations)+1)
	for key, value := range annotations {
		mergedAnnotations[key] = value
	}
	if marker == "" {
		mergedAnnotations[nodeInstanceAnnotation] = instanceID
	}
	needsPatch := !stringMapEqual(labels, mergedLabels) || !stringMapEqual(annotations, mergedAnnotations) || !taintsEqual(node.Spec.Taints, mergedTaints)
	if needsPatch {
		metadata := map[string]any{
			"labels":      mergedLabels,
			"annotations": mergedAnnotations,
		}
		if node.ObjectMeta.ResourceVersion != "" {
			metadata["resourceVersion"] = node.ObjectMeta.ResourceVersion
		}
		patch := map[string]any{
			"metadata": metadata,
			"spec":     map[string]any{"taints": mergedTaints},
		}
		body, err = client.PatchJSON(ctx, path, patch)
		if err != nil {
			return nil, fmt.Errorf("patch owned Node %q: %w", name, err)
		}
		if err := json.Unmarshal(body, &node); err != nil {
			return nil, fmt.Errorf("decode patched Node %q: %w", name, err)
		}
		if err := verifyNodeOwnership(&node, name, instanceID, verifyUID, false); err != nil {
			return nil, err
		}
	}
	return &node, nil
}

func stringMapEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func taintsEqual(left, right []Taint) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Key != right[index].Key || left[index].Value != right[index].Value || left[index].Effect != right[index].Effect || !timeEqual(left[index].TimeAdded, right[index].TimeAdded) {
			return false
		}
	}
	return true
}

func timeEqual(left, right *metav1.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Time.Equal(right.Time)
}
