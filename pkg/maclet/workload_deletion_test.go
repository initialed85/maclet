package maclet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReconcileFinishesPodDeletion pins the order a deletion is acknowledged in, which is
// the order kubelet uses: stop the workload, report a terminal phase, then delete the Pod
// object. Before this, removal depended only on the backstop sweeper's stale window (45s
// on the live node), and that is long enough for a Deployment teardown to keep ReplicaSets
// creating Pods while their predecessors wait to be swept -- observed as three generations
// coexisting during maclet-lifecycle teardown on icarus-2.local.
//
// The case that matters most is a Pod maclet never ran: it has no workload to stop, so
// nothing used to touch it, and its deletion sat until the sweeper. It also must not be
// described as "stopped", because it never was.
func TestReconcileFinishesPodDeletion(t *testing.T) {
	var (
		deletedPath string
		statusPhase string
		statusMsg   string
		eventReason string
	)
	binary := filepath.Join(t.TempDir(), "macker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/status"):
			body, _ := io.ReadAll(request.Body)
			submitted := &Pod{}
			if err := json.Unmarshal(body, submitted); err != nil {
				t.Errorf("decode status submission: %v", err)
			}
			statusPhase = string(submitted.Status.Phase)
			statusMsg = submitted.Status.Message
			submitted.ObjectMeta.ResourceVersion = "9"
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(submitted)
		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/events"):
			body, _ := io.ReadAll(request.Body)
			filed := &Event{}
			_ = json.Unmarshal(body, filed)
			eventReason = filed.Reason
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(&Event{})
		case request.Method == http.MethodDelete:
			deletedPath = request.URL.Path + "?" + request.URL.RawQuery
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(&Pod{})
		default:
			http.NotFound(response, request)
		}
	}))
	manager := newWorkloadManagerWithState(nil, binary, "192.0.2.10", t.TempDir())
	manager.apiClient = client
	pod := Pod{
		ObjectMeta: ObjectMeta{
			Namespace:         "demo",
			Name:              "gone",
			UID:               "uid-gone",
			DeletionTimestamp: testDeletionTimestamp("2026-11-25T09:00:00Z"),
		},
		Spec: PodSpec{NodeName: "icarus-2.local"},
	}
	if err := manager.reconcile(context.Background(), client, []Pod{pod}); err != nil {
		t.Fatalf("reconcile a deleted Pod: %v", err)
	}
	if !strings.Contains(deletedPath, "/api/v1/namespaces/demo/pods/gone") || !strings.Contains(deletedPath, "gracePeriodSeconds=0") {
		t.Fatalf("Pod object deletion = %q, want maclet to remove it after acknowledging", deletedPath)
	}
	if statusPhase != "Failed" {
		t.Fatalf("acknowledgement phase = %q, want Failed", statusPhase)
	}
	// PodStatus.reason is deliberately maclet's Pending-state field, mirroring
	// Kubernetes; a terminal Pod's named reason travels as an Event.
	if eventReason != "MacletWorkloadDeleted" {
		t.Fatalf("acknowledgement Event reason = %q, want MacletWorkloadDeleted", eventReason)
	}
	if !strings.Contains(statusMsg, "never ran") {
		t.Errorf("a workload maclet never started must not be reported as stopped: %q", statusMsg)
	}
}

// TestReconcileLeavesALivePodAlone is the other half: a Pod without a deletion timestamp
// is never deleted by the acknowledgement path, whatever else reconcile decides about it.
func TestReconcileLeavesALivePodAlone(t *testing.T) {
	var deletedPath string
	binary := filepath.Join(t.TempDir(), "macker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/status"):
			body, _ := io.ReadAll(request.Body)
			submitted := &Pod{}
			_ = json.Unmarshal(body, submitted)
			submitted.ObjectMeta.ResourceVersion = "9"
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(submitted)
		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/events"):
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(&Event{})
		case request.Method == http.MethodDelete:
			deletedPath = request.URL.Path
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(&Pod{})
		default:
			http.NotFound(response, request)
		}
	}))
	manager := newWorkloadManagerWithState(nil, binary, "192.0.2.10", t.TempDir())
	manager.apiClient = client
	pod := Pod{
		ObjectMeta: ObjectMeta{Namespace: "demo", Name: "alive", UID: "uid-alive", Labels: map[string]string{"example.test/owner": "someone-else"}},
		Spec:       PodSpec{NodeName: "icarus-2.local"},
	}
	if err := manager.reconcile(context.Background(), client, []Pod{pod}); err != nil {
		t.Fatalf("reconcile a live unlabelled Pod: %v", err)
	}
	if deletedPath != "" {
		t.Fatalf("maclet deleted a Pod that was not being deleted: %s", deletedPath)
	}
}
