package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMackerContainerStatus(t *testing.T) {
	output := "NAME\tIMAGE\tSTATUS\tNETWORK\tPID\tPORTS\tCREATED\n" +
		"maclet-default-web-uid\tdocker.io/example/web:latest\trunning\tbridge101:10.42.8.3\t1234\t-\t2026-08-30T09:00:00Z\n"
	if status, found := mackerContainerStatus(output, "maclet-default-web-uid"); !found || status != "running" {
		t.Fatalf("mackerContainerStatus() = (%q, %v), want (running, true)", status, found)
	}
	if status, found := mackerContainerStatus(output, "missing"); found || status != "" {
		t.Fatalf("mackerContainerStatus(missing) = (%q, %v), want (empty, false)", status, found)
	}
}

func TestWorkloadContainerName(t *testing.T) {
	pod := Pod{Metadata: ObjectMeta{Namespace: "Demo_Namespace", Name: "hello.world", UID: "ABC-123"}}
	name := workloadContainerName(pod)
	if name != "maclet-demo_namespace-hello.world-abc-123" {
		t.Fatalf("workloadContainerName() = %q", name)
	}
	long := Pod{Metadata: ObjectMeta{Namespace: "namespace", Name: "pod", UID: "012345678901234567890123456789012345678901234567890123456789012345678901234567890"}}
	if got := workloadContainerName(long); len(got) > 120 || got == "" {
		t.Fatalf("long workloadContainerName() = %q (len=%d)", got, len(got))
	}
}

func TestWorkloadRunArgs(t *testing.T) {
	manager := newWorkloadManager(&DarwinNetworkHandle{Interface: "bridge101", PodCIDR: "10.42.8.0/24", Gateway: "10.42.8.2"}, "/usr/local/bin/macker", "192.168.137.111")
	pod := Pod{
		Metadata: ObjectMeta{Namespace: "default", Name: "web", UID: "uid-1"},
		Spec: PodSpec{Containers: []ContainerSpec{{
			Name: "web", Image: "initialed85/nginx-darwin:latest",
			Command: []string{"/bin/nginx"}, Args: []string{"-g", "daemon off;"},
			Ports: []ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}},
		}}},
	}
	managed := &managedWorkload{ContainerName: "maclet-default-web-uid-1", IP: "10.42.8.3"}
	args, err := manager.runArgs(pod, pod.Spec.Containers[0], managed)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--detach", "--net=external", "--interface", "bridge101", "--ip", "10.42.8.3", "--host-interface", "bridge101", "--host-ip", "10.42.8.1", "--name", "maclet-default-web-uid-1", "--env", "MACKER_PORT_1=8080", "--entrypoint", "/bin/nginx", "initialed85/nginx-darwin:latest", "--", "-g", "daemon off;"}
	if len(args) != len(want) {
		t.Fatalf("run args length = %d, want %d: %#v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("run args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	pod.Spec.Containers[0].Ports[0].HostPort = 8080
	if _, err := manager.runArgs(pod, pod.Spec.Containers[0], managed); err == nil {
		t.Fatal("runArgs accepted unsupported hostPort")
	}
}

func TestDesiredPodStatus(t *testing.T) {
	pod := Pod{Metadata: ObjectMeta{Name: "web", Namespace: "default"}, Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}, Status: PodStatus{Phase: "Pending", Conditions: []PodCondition{{Type: "PodScheduled", Status: "False"}}}}
	status := desiredPodStatus(pod, "192.168.137.111", "Running", "10.42.8.3", "MacletWorkloadRunning", "running", true, 2)
	if status.Phase != "Running" || status.PodIP != "10.42.8.3" || status.HostIP != "192.168.137.111" {
		t.Fatalf("status identity = %#v", status)
	}
	if len(status.ContainerStatuses) != 1 || !status.ContainerStatuses[0].Ready || status.ContainerStatuses[0].RestartCount != 2 {
		t.Fatalf("container status = %#v", status.ContainerStatuses)
	}
	for _, condition := range status.Conditions {
		if condition.Type == "PodScheduled" && condition.Status != "True" {
			t.Fatalf("PodScheduled condition not updated: %#v", status.Conditions)
		}
	}
}

func TestUpdatePodStatusPreservesMetadata(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api/v1/namespaces/default/pods/web/status" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload struct {
			Metadata ObjectMeta `json:"metadata"`
			Status   PodStatus  `json:"status"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload.Metadata.Labels["k8s-darwin.dev/native"] != "true" {
			t.Errorf("labels = %#v", payload.Metadata.Labels)
		}
		if payload.Metadata.Annotations["example.test/owner"] != "maclet-test" {
			t.Errorf("annotations = %#v", payload.Metadata.Annotations)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(Pod{Metadata: payload.Metadata, Status: payload.Status})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	pod := &Pod{Metadata: ObjectMeta{
		Name: "web", Namespace: "default", UID: "uid-1", ResourceVersion: "7",
		Labels:      map[string]string{"k8s-darwin.dev/native": "true"},
		Annotations: map[string]string{"example.test/owner": "maclet-test"},
	}, Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}}
	desired := desiredPodStatus(*pod, "192.168.137.111", "Running", "10.42.8.3", "MacletWorkloadRunning", "running", true, 0)
	updated, err := updatePodStatus(context.Background(), client, pod, desired)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Metadata.ResourceVersion != "7" || updated.Status.PodIP != "10.42.8.3" {
		t.Fatalf("updated Pod = %#v", updated)
	}
}
