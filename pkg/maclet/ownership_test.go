package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func ownershipTestNode(name, uid, instanceID string) Node {
	node := desiredNode(name, "192.0.2.10")
	node.ObjectMeta.UID = types.UID(uid)
	if instanceID != "" {
		node.ObjectMeta.Annotations = map[string]string{nodeInstanceAnnotation: instanceID}
	}
	return node
}

func ownershipTestClient(t *testing.T, handler http.Handler) *APIClient {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestEnsureStateInstanceIDPersistsRandomIdentity(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := &LocalState{Version: 1, NodeName: "maclet"}
	if err := ensureStateInstanceID(state, statePath); err != nil {
		t.Fatal(err)
	}
	if len(state.InstanceID) != 32 {
		t.Fatalf("instance ID length = %d, want 32 hex characters", len(state.InstanceID))
	}
	body, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted LocalState
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.InstanceID != state.InstanceID {
		t.Fatalf("persisted instance ID = %q, want %q", persisted.InstanceID, state.InstanceID)
	}
	oldID := state.InstanceID
	if err := ensureStateInstanceID(state, statePath); err != nil {
		t.Fatal(err)
	}
	if state.InstanceID != oldID {
		t.Fatalf("existing instance ID changed from %q to %q", oldID, state.InstanceID)
	}
}

func TestEnsureOwnedNodeCreatesWithInstanceMarker(t *testing.T) {
	const instanceID = "instance-created"
	var created bool
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet:
			http.NotFound(response, request)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/nodes":
			var node Node
			if err := json.NewDecoder(request.Body).Decode(&node); err != nil {
				t.Errorf("decode create Node: %v", err)
			}
			if node.ObjectMeta.Annotations[nodeInstanceAnnotation] != instanceID {
				t.Errorf("create marker = %#v, want %q", node.ObjectMeta.Annotations, instanceID)
			}
			created = true
			_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-created", instanceID))
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	node, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", instanceID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !created || node.ObjectMeta.UID != "uid-created" || node.ObjectMeta.Annotations[nodeInstanceAnnotation] != instanceID {
		t.Fatalf("created Node = %#v", node)
	}
}

func TestEnsureOwnedNodeRequiresExplicitAdoption(t *testing.T) {
	var patched bool
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPatch {
			patched = true
		}
		_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-existing", ""))
	}))
	_, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-new", "", false)
	if !errors.Is(err, errNodeNameConflict) {
		t.Fatalf("unmarked Node error = %v, want name conflict", err)
	}
	if patched {
		t.Fatal("unmarked Node was patched without explicit adoption")
	}
}

func TestEnsureOwnedNodeRejectsIncompatibleAdoption(t *testing.T) {
	var patched bool
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPatch {
			patched = true
		}
		node := ownershipTestNode("maclet", "uid-linux", "")
		node.ObjectMeta.Labels["kubernetes.io/os"] = "linux"
		_ = json.NewEncoder(response).Encode(node)
	}))
	_, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-adopt", "", true)
	if !errors.Is(err, errNodeNameConflict) {
		t.Fatalf("incompatible adoption error = %v, want name conflict", err)
	}
	if patched {
		t.Fatal("incompatible Node was patched")
	}
}

func TestEnsureOwnedNodeAdoptsCompatibleUnmarkedNode(t *testing.T) {
	var patched bool
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-adopt", ""))
		case http.MethodPatch:
			var patch map[string]any
			if err := json.NewDecoder(request.Body).Decode(&patch); err != nil {
				t.Errorf("decode adoption patch: %v", err)
			}
			metadata := patch["metadata"].(map[string]any)
			annotations := metadata["annotations"].(map[string]any)
			if annotations[nodeInstanceAnnotation] != "instance-adopt" {
				t.Errorf("adoption marker = %#v", annotations)
			}
			patched = true
			_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-adopt", "instance-adopt"))
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	node, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-adopt", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !patched || node.ObjectMeta.Annotations[nodeInstanceAnnotation] != "instance-adopt" {
		t.Fatalf("adopted Node = %#v", node)
	}
}

func TestEnsureOwnedNodeRejectsForeignIdentityAndUID(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		expectedUID string
	}{
		{name: "foreign marker", instanceID: "instance-foreign", expectedUID: ""},
		{name: "different uid", instanceID: "instance-owned", expectedUID: "uid-other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var patched bool
			client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodPatch {
					patched = true
				}
				marker := "instance-owned"
				if test.name == "foreign marker" {
					marker = "instance-foreign"
				}
				_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-current", marker))
			}))
			_, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-owned", test.expectedUID, false)
			if !errors.Is(err, errNodeNameConflict) {
				t.Fatalf("Node error = %v, want name conflict", err)
			}
			if patched {
				t.Fatal("conflicting Node was patched")
			}
		})
	}
}

func TestEnsureOwnedNodeRecreatesDeletedOwnedNode(t *testing.T) {
	var postCount int
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			http.NotFound(response, request)
		case http.MethodPost:
			postCount++
			_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-recreated", "instance-owned"))
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	node, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-owned", "uid-deleted", false)
	if err != nil {
		t.Fatal(err)
	}
	if postCount != 1 || string(node.ObjectMeta.UID) != "uid-recreated" {
		t.Fatalf("recreated Node = %#v, POST count = %d", node, postCount)
	}
}

func TestEnsureOwnedNodeNeverAdoptsPostConflictWithoutMarker(t *testing.T) {
	var postCount, patchCount int
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if postCount == 0 {
				http.NotFound(response, request)
				return
			}
			_ = json.NewEncoder(response).Encode(ownershipTestNode("maclet", "uid-foreign", "other-instance"))
		case http.MethodPost:
			postCount++
			response.WriteHeader(http.StatusConflict)
		case http.MethodPatch:
			patchCount++
			response.WriteHeader(http.StatusOK)
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	_, err := ensureOwnedNode(context.Background(), client, "maclet", "192.0.2.10", "instance-owned", "", true)
	if !errors.Is(err, errNodeNameConflict) {
		t.Fatalf("post-conflict error = %v, want name conflict", err)
	}
	if postCount != 1 || patchCount != 0 {
		t.Fatalf("post/patch counts = %d/%d, want 1/0", postCount, patchCount)
	}
}
