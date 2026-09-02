package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateNodeStatusRetriesWithRefreshedSpecAfterTaintRestriction(t *testing.T) {
	managedTaint := corev1.Taint{Key: managedTaintKey, Value: managedTaintValue, Effect: managedTaintEffect}
	node := &Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: ObjectMeta{
			Name:            "maclet",
			UID:             "node-uid",
			ResourceVersion: "7",
			Labels:          map[string]string{managedLabelKey: managedLabelValue},
			Annotations:     map[string]string{"example.test/owned": "true"},
		},
		Spec: NodeSpec{
			PodCIDR: "10.42.4.0/24",
			Taints:  []Taint{managedTaint},
		},
	}
	var requests []map[string]json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/nodes/maclet" {
			refreshed := *node
			refreshed.Spec.PodCIDR = "10.42.5.0/24"
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&refreshed); err != nil {
				t.Fatalf("encode refreshed Node: %v", err)
			}
			return
		}
		if request.Method != http.MethodPut || request.URL.Path != "/api/v1/nodes/maclet/status" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, payload)
		if len(requests) <= 2 {
			response.WriteHeader(http.StatusForbidden)
			_, _ = response.Write([]byte(`nodes "maclet" is not allowed to modify taints`))
			return
		}
		var submittedSpec NodeSpec
		if err := json.Unmarshal(payload["spec"], &submittedSpec); err != nil {
			t.Fatalf("decode status retry spec: %v", err)
		}
		if len(submittedSpec.Taints) != 1 || submittedSpec.Taints[0] != managedTaint {
			t.Fatalf("status retry taints = %#v, want managed taint", submittedSpec.Taints)
		}
		if submittedSpec.PodCIDR != "10.42.5.0/24" {
			t.Fatalf("status retry used stale PodCIDR %q", submittedSpec.PodCIDR)
		}
		var updated Node = *node
		updated.ResourceVersion = "8"
		updated.Status = nodeStatus(node.Name, "192.168.137.111", "", time.Now())
		response.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(response).Encode(&updated); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updateNodeStatus(context.Background(), client, node, "192.168.137.111", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("status requests = %d, want 3", len(requests))
	}
	var metadata map[string]any
	if err := json.Unmarshal(requests[0]["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["uid"] != "node-uid" {
		t.Fatalf("first status metadata UID = %#v", metadata["uid"])
	}
	if len(updated.Spec.Taints) != 1 || updated.Spec.Taints[0] != managedTaint {
		t.Fatalf("updated taints = %#v, want managed taint preserved", updated.Spec.Taints)
	}
}

func TestIsNodeTaintRestrictionError(t *testing.T) {
	err := &HTTPError{Code: http.StatusForbidden, Body: `nodes "maclet" is not allowed to modify taints`}
	if !isNodeTaintRestrictionError(err) {
		t.Fatal("expected NodeRestriction taint error")
	}
	if isNodeTaintRestrictionError(&HTTPError{Code: http.StatusForbidden, Body: "forbidden"}) {
		t.Fatal("generic forbidden error was treated as taint restriction")
	}
	if isNodeTaintRestrictionError(&HTTPError{Code: http.StatusConflict, Body: "not allowed to modify taints"}) {
		t.Fatal("conflict was treated as taint restriction")
	}
	if isNodeTaintRestrictionError(errors.New("not an API error")) {
		t.Fatal("non-API error was treated as taint restriction")
	}
}
