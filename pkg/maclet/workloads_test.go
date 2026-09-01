package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func testHostPathVolume(name, path, volumeType string) Volume {
	hostPathType := corev1.HostPathType(volumeType)
	return Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &HostPathVolumeSource{Path: path, Type: &hostPathType}}}
}

func TestWorkloadJournalRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	manager := newWorkloadManagerWithState(nil, "", "", stateDir)
	manager.workloads["uid-1"] = &managedWorkload{
		UID: "uid-1", Namespace: "default", Name: "web",
		ContainerName: "maclet-default-web-uid-1", IP: "10.42.8.3", RestartCount: 2,
	}
	if err := manager.persistJournal(); err != nil {
		t.Fatal(err)
	}
	loaded := newWorkloadManagerWithState(nil, "", "", stateDir)
	if err := loaded.loadJournal(); err != nil {
		t.Fatal(err)
	}
	workload := loaded.workloads["uid-1"]
	if workload == nil || workload.Namespace != "default" || workload.Name != "web" || workload.ContainerName != "maclet-default-web-uid-1" || workload.IP != "10.42.8.3" || workload.RestartCount != 2 {
		t.Fatalf("loaded workload = %#v", workload)
	}
	body, err := os.ReadFile(filepath.Join(stateDir, "workloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal workloadJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.Version != 1 || len(journal.Workloads) != 1 {
		t.Fatalf("journal = %#v", journal)
	}
}

func TestMackerImageMissing(t *testing.T) {
	if !mackerImageMissing(errors.New("run image: image is not present in local storage; pull or build it first")) {
		t.Fatal("mackerImageMissing() did not recognize the current Macker error")
	}
	if mackerImageMissing(errors.New("run image: permission denied")) {
		t.Fatal("mackerImageMissing() recognized an unrelated error")
	}
}

func TestStartMackerWorkloadPullsMissingImageAndRetries(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "calls")
	markerPath := filepath.Join(root, "pulled")
	scriptPath := filepath.Join(root, "macker")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> \"" + logPath + "\"\n" +
		"if [ \"$1\" = run ] && [ ! -f \"" + markerPath + "\" ]; then\n" +
		"  echo 'image is not present in local storage; pull or build it first' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = pull ]; then touch \"" + markerPath + "\"; fi\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, scriptPath, "")
	if err := manager.startMackerWorkload([]string{"run", "--detach", "example/image:latest"}, "example/image:latest"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(calls)), "run --detach example/image:latest\npull example/image:latest\nrun --detach example/image:latest"; got != want {
		t.Fatalf("Macker calls = %q, want %q", got, want)
	}
}

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

func TestDarwinNetworkPeerGatewayReservation(t *testing.T) {
	network := &DarwinNetworkHandle{
		PodCIDR:      "10.42.8.0/24",
		Gateway:      "10.42.8.2",
		PeerGateways: []DarwinPeerGateway{{Gateway: "10.42.8.254"}},
	}
	if got, err := network.firstAvailableWorkloadIP(map[string]bool{}); err != nil || got != "10.42.8.3" {
		t.Fatalf("firstAvailableWorkloadIP() = %q, %v; want 10.42.8.3", got, err)
	}
	if err := network.validateWorkloadAddress("10.42.8.254"); err == nil {
		t.Error("validateWorkloadAddress accepted a peer gateway")
	}
}

func TestWorkloadContainerName(t *testing.T) {
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "Demo_Namespace", Name: "hello.world", UID: "ABC-123"}}
	name := workloadContainerName(pod)
	if name != "maclet-demo_namespace-hello.world-abc-123" {
		t.Fatalf("workloadContainerName() = %q", name)
	}
	long := Pod{ObjectMeta: ObjectMeta{Namespace: "namespace", Name: "pod", UID: "012345678901234567890123456789012345678901234567890123456789012345678901234567890"}}
	if got := workloadContainerName(long); len(got) > 120 || got == "" {
		t.Fatalf("long workloadContainerName() = %q (len=%d)", got, len(got))
	}
}

func TestWorkloadRunArgs(t *testing.T) {
	manager := newWorkloadManager(&DarwinNetworkHandle{Interface: "bridge101", PodCIDR: "10.42.8.0/24", Gateway: "10.42.8.2"}, "/usr/local/bin/macker", "192.168.137.111")
	pod := Pod{
		ObjectMeta: ObjectMeta{
			Namespace: "default", Name: "web", UID: "uid-1",
			Annotations: map[string]string{nativeDisablePortForwardAnnotation: "true"},
		},
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
	pod.Spec.Containers[0].Ports[0].HostPort = 0
	delete(pod.ObjectMeta.Annotations, nativeDisablePortForwardAnnotation)
	args, err = manager.runArgs(pod, pod.Spec.Containers[0], managed)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "-p\x008080:auto/tcp") || strings.Contains(joined, "MACKER_PORT_1=8080") {
		t.Fatalf("port-forward run args = %#v", args)
	}
	pod.Spec.Containers[0].Ports[0].Protocol = "UDP"
	args, err = manager.runArgs(pod, pod.Spec.Containers[0], managed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, "\x00"), "-p\x008080:auto/udp") {
		t.Fatalf("UDP port-forward run args = %#v", args)
	}
}

func TestMackerVolumeArgs(t *testing.T) {
	root := t.TempDir()
	content := filepath.Join(root, "content")
	if err := os.Mkdir(content, 0o755); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(content, "index.html")
	if err := os.WriteFile(index, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.txt")
	if err := os.WriteFile(config, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}
	pod := Pod{Spec: PodSpec{
		Volumes: []Volume{
			testHostPathVolume("content", content, "Directory"),
			testHostPathVolume("config", config, "File"),
		},
	}}
	container := ContainerSpec{VolumeMounts: []VolumeMount{
		{Name: "content", MountPath: "/usr/share/nginx/html/index.html", SubPath: "index.html"},
		{Name: "config", MountPath: "/etc/example/config.txt"},
	}}
	args, err := mackerVolumeArgs(pod, container)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-v", index + ":/usr/share/nginx/html/index.html", "-v", config + ":/etc/example/config.txt"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mackerVolumeArgs() = %#v, want %#v", args, want)
	}
}

func TestMackerVolumeArgsRejectsUnsupportedMounts(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{
		testHostPathVolume("existing", existing, "Directory"),
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "existing", MountPath: "/data", ReadOnly: true}}}); err == nil || !strings.Contains(err.Error(), "readOnly") {
		t.Fatalf("readOnly mount error = %v, want readOnly rejection", err)
	}
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{{Name: "projected"}}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "projected", MountPath: "/data"}}}); err == nil || !strings.Contains(err.Error(), "supported hostPath") {
		t.Fatalf("non-hostPath mount error = %v, want unsupported hostPath error", err)
	}
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{
		testHostPathVolume("existing", existing, "Directory"),
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "existing", MountPath: "/data", SubPathExpr: "$(POD_NAME)"}}}); err == nil || !strings.Contains(err.Error(), "subPathExpr") {
		t.Fatalf("subPathExpr mount error = %v, want subPathExpr rejection", err)
	}
	missing := filepath.Join(root, "created", "directory")
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{
		testHostPathVolume("created", missing, "DirectoryOrCreate"),
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "created", MountPath: "/data"}}}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(missing); err != nil || !info.IsDir() {
		t.Fatalf("DirectoryOrCreate path = %v, info = %#v", err, info)
	}
}

func TestMackerVolumeArgsRejectsSubPathEscape(t *testing.T) {
	root := t.TempDir()
	volumeRoot := filepath.Join(root, "volume")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(volumeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(volumeRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	pod := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("volume", volumeRoot, "Directory")}}}
	_, err := mackerVolumeArgs(pod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "volume", MountPath: "/data", SubPath: "escape/secret"}}})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("subPath escape error = %v", err)
	}
}

func TestDesiredPodStatus(t *testing.T) {
	pod := Pod{ObjectMeta: ObjectMeta{Name: "web", Namespace: "default"}, Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}, Status: PodStatus{Phase: "Pending", Conditions: []PodCondition{{Type: "PodScheduled", Status: "False"}}}}
	status := desiredPodStatus(pod, "192.168.137.111", "Running", "10.42.8.3", "MacletWorkloadRunning", "running", true, 2)
	if status.Phase != "Running" || status.Reason != "" || status.PodIP != "10.42.8.3" || status.HostIP != "192.168.137.111" {
		t.Fatalf("status identity = %#v", status)
	}
	if len(status.ContainerStatuses) != 1 || !status.ContainerStatuses[0].Ready || status.ContainerStatuses[0].RestartCount != 2 {

		t.Fatalf("container status = %#v", status.ContainerStatuses)
	}
	for _, condition := range status.Conditions {
		if condition.Type == "PodScheduled" && (condition.Status != "True" || condition.Reason != "PodScheduled") {
			t.Fatalf("PodScheduled condition not updated: %#v", status.Conditions)
		}
		if (condition.Type == "Ready" || condition.Type == "ContainersReady") && (condition.Status != "True" || condition.Reason != "ContainersReady") {
			t.Fatalf("ready condition not updated: %#v", status.Conditions)
		}
	}

}

func TestDesiredPodStatusUsesStandardPendingReasons(t *testing.T) {
	pod := Pod{Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}}
	status := desiredPodStatus(pod, "192.168.137.111", "Pending", "10.42.8.3", "MacletUnsupportedPod", "unsupported configuration", false, 0)
	if status.Reason != "" {
		t.Fatalf("Pod status reason = %q, want empty", status.Reason)
	}
	if waiting := status.ContainerStatuses[0].State.Waiting; waiting == nil || waiting.Reason != "CreateContainerConfigError" {
		t.Fatalf("waiting status = %#v", status.ContainerStatuses[0].State.Waiting)
	}
	for _, condition := range status.Conditions {
		if (condition.Type == "Ready" || condition.Type == "ContainersReady") && (condition.Status != "False" || condition.Reason != "ContainersNotReady") {
			t.Fatalf("not-ready condition = %#v", condition)
		}
	}
}

func TestDesiredPodStatusIncludesMackerExit(t *testing.T) {
	started := time.Date(2026, 8, 30, 18, 0, 0, 0, time.UTC)
	finished := started.Add(time.Second)
	exitCode := int32(17)
	inspection := &mackerInspection{
		ExitCode: &exitCode, StartedAt: &started, FinishedAt: &finished,
		TerminationReason: "exited-with-error", TerminationSignal: "",
	}
	pod := Pod{ObjectMeta: ObjectMeta{Name: "web", Namespace: "default"}, Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}}
	status := desiredPodStatus(pod, "192.168.137.111", "Failed", "10.42.8.3", "MacletWorkloadFailed", "Macker workload exited with code 17", false, 0, inspection)
	if len(status.ContainerStatuses) != 1 {
		t.Fatalf("container statuses = %#v", status.ContainerStatuses)
	}
	terminated := status.ContainerStatuses[0].State.Terminated
	if status.Reason != "" {
		t.Fatalf("Pod status reason = %q, want empty", status.Reason)
	}
	if terminated == nil || terminated.Reason != "Error" || terminated.ExitCode != 17 || !terminated.StartedAt.Time.Equal(started) || !terminated.FinishedAt.Time.Equal(finished) {
		t.Fatalf("terminated status = %#v", terminated)
	}
	if !strings.Contains(terminated.Message, "exited-with-error") {
		t.Fatalf("terminated message = %q", terminated.Message)
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
		_ = json.NewEncoder(response).Encode(Pod{ObjectMeta: payload.Metadata, Status: payload.Status})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	pod := &Pod{ObjectMeta: ObjectMeta{
		Name: "web", Namespace: "default", UID: "uid-1", ResourceVersion: "7",
		Labels:      map[string]string{"k8s-darwin.dev/native": "true"},
		Annotations: map[string]string{"example.test/owner": "maclet-test"},
	}, Spec: PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}}}
	desired := desiredPodStatus(*pod, "192.168.137.111", "Running", "10.42.8.3", "MacletWorkloadRunning", "running", true, 0)
	updated, err := updatePodStatus(context.Background(), client, pod, desired)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ObjectMeta.ResourceVersion != "7" || updated.Status.PodIP != "10.42.8.3" {
		t.Fatalf("updated Pod = %#v", updated)
	}
}
