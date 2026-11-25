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
	"sync/atomic"
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
		VolumePaths: []string{filepath.Join(stateDir, "volumes", "web", "settings")},
		NFSMounts:   []nfsMountRecord{{Target: filepath.Join(stateDir, "nfs", "web", "data"), Spec: nfsMountSpec{Server: "192.0.2.25", Share: "/exports/data", ReadOnly: true}}},
	}
	if err := manager.persistJournal(); err != nil {
		t.Fatal(err)
	}
	loaded := newWorkloadManagerWithState(nil, "", "", stateDir)
	if err := loaded.loadJournal(); err != nil {
		t.Fatal(err)
	}
	workload := loaded.workloads["uid-1"]
	if workload == nil || workload.Namespace != "default" || workload.Name != "web" || workload.ContainerName != "maclet-default-web-uid-1" || workload.IP != "10.42.8.3" || workload.RestartCount != 2 || len(workload.VolumePaths) != 1 || len(workload.NFSMounts) != 1 {
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
			Name: "web", Image: "initialed85/nginx-darwin:latest", WorkingDir: "/app/../work",
			Command: []string{"/bin/nginx"}, Args: []string{"-g", "daemon off;"},
			Ports: []ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}},
		}}},
	}
	managed := &managedWorkload{ContainerName: "maclet-default-web-uid-1", IP: "10.42.8.3"}
	args, err := manager.runArgs(pod, pod.Spec.Containers[0], managed)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--detach", "--net=external", "--interface", "bridge101", "--ip", "10.42.8.3", "--host-interface", "bridge101", "--host-ip", "10.42.8.1", "--name", "maclet-default-web-uid-1", "--workdir", "/work", "--env", "MACKER_PORT_1=8080", "--entrypoint", "/bin/nginx", "initialed85/nginx-darwin:latest", "--", "-g", "daemon off;"}
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
	pod.Spec.Containers[0].WorkingDir = "relative/workdir"
	if _, err := manager.runArgs(pod, pod.Spec.Containers[0], managed); err == nil || !strings.Contains(err.Error(), "workingDir") {
		t.Fatalf("runArgs accepted relative workingDir: %v", err)
	}
	pod.Spec.Containers[0].WorkingDir = "/app/../work"
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

func TestHostNetworkRunArgs(t *testing.T) {
	manager := newWorkloadManager(nil, "/usr/local/bin/macker", "192.0.2.10")
	// No nativeDisablePortForward annotation: a hostNetwork Pod binds its declared
	// port on the host, which is ordinary Kubernetes behaviour, so the direct
	// shape is the default here rather than something a manifest must opt into.
	// HostPort mirrors what the API server writes for a hostNetwork Pod; rejecting
	// it would make every hostNetwork Pod with ports impossible on this node class.
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "default", Name: "host", UID: "uid-host"}, Spec: PodSpec{HostNetwork: true, DNSPolicy: "ClusterFirstWithHostNet", Containers: []ContainerSpec{{Name: "web", Image: "example/native", Ports: []ContainerPort{{ContainerPort: 8080, HostPort: 8080}, {ContainerPort: 9093, Protocol: "UDP"}}, Command: []string{"/app/server"}}}}}
	managed := &managedWorkload{ContainerName: "maclet-default-host-uid-host", IP: "192.0.2.10", HostNetwork: true}
	args, err := manager.runArgs(pod, pod.Spec.Containers[0], managed)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "run\x00--detach\x00--net=host") || strings.Contains(joined, "--net=external") {
		t.Fatalf("hostNetwork args = %#v", args)
	}
	if !strings.Contains(joined, "MACKER_PORT_1=8080") || !strings.Contains(joined, "MACKER_PORT_2=9093") {
		t.Fatalf("hostNetwork args did not publish the declared ports directly: %#v", args)
	}
	for _, arg := range args {
		if arg == "-p" || strings.HasPrefix(arg, "auto/") {
			t.Fatalf("hostNetwork args requested a PF mapping, which would move the listener off the declared port: %#v", args)
		}
	}
	// A hostPort that is not the hostNetwork default is still a mapping request.
	hostPortOnly := Pod{ObjectMeta: ObjectMeta{Namespace: "default", Name: "host", UID: "uid-host"}, Spec: PodSpec{HostNetwork: true, DNSPolicy: "ClusterFirstWithHostNet", Containers: []ContainerSpec{{Name: "web", Image: "example/native", Ports: []ContainerPort{{ContainerPort: 8080, HostPort: 9999}}}}}}
	if _, err := manager.runArgs(hostPortOnly, hostPortOnly.Spec.Containers[0], managed); err == nil || !strings.Contains(err.Error(), "hostPort 9999") {
		t.Fatalf("hostNetwork args accepted a real host-port mapping: %v", err)
	}
}

func TestNativePodNetworkValidation(t *testing.T) {
	badHost := Pod{ObjectMeta: ObjectMeta{Name: "host"}, Spec: PodSpec{HostNetwork: true, DNSPolicy: "ClusterFirst"}}
	if err := validateNativePodNetwork(badHost); err == nil {
		t.Fatal("hostNetwork accepted ClusterFirst DNS policy")
	}
	badPod := Pod{ObjectMeta: ObjectMeta{Name: "pod"}, Spec: PodSpec{DNSPolicy: "ClusterFirstWithHostNet"}}
	if err := validateNativePodNetwork(badPod); err == nil {
		t.Fatal("non-hostNetwork Pod accepted ClusterFirstWithHostNet")
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
	args, err := mackerVolumeArgs(pod, container, false)
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
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "existing", MountPath: "/data", ReadOnly: true}}}, false); err == nil || !strings.Contains(err.Error(), "readOnly") {
		t.Fatalf("readOnly mount error = %v, want readOnly rejection", err)
	}
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{{Name: "projected"}}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "projected", MountPath: "/data"}}}, false); err == nil || !strings.Contains(err.Error(), "supported hostPath") {
		t.Fatalf("non-hostPath mount error = %v, want unsupported hostPath error", err)
	}
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{
		testHostPathVolume("existing", existing, "Directory"),
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "existing", MountPath: "/data", SubPathExpr: "$(POD_NAME)"}}}, false); err == nil || !strings.Contains(err.Error(), "subPathExpr") {
		t.Fatalf("subPathExpr mount error = %v, want subPathExpr rejection", err)
	}
	missing := filepath.Join(root, "created", "directory")
	if _, err := mackerVolumeArgs(Pod{Spec: PodSpec{Volumes: []Volume{
		testHostPathVolume("created", missing, "DirectoryOrCreate"),
	}}}, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "created", MountPath: "/data"}}}, false); err != nil {
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
	_, err := mackerVolumeArgs(pod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "volume", MountPath: "/data", SubPath: "escape/secret"}}}, false)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("subPath escape error = %v", err)
	}
}

func TestMackerVolumeArgsPassesReadOnlyWhenMackerSupportsIt(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "secrets")
	if err := os.Mkdir(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volume, "file.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "config.txt")
	if err := os.WriteFile(config, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}
	pod := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("data", volume, "Directory")}}}
	container := ContainerSpec{VolumeMounts: []VolumeMount{
		{Name: "data", MountPath: "/data", ReadOnly: true},
		{Name: "data", MountPath: "/rw"},
	}}
	args, err := mackerVolumeArgs(pod, container, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-v", volume + ":/data:ro", "-v", volume + ":/rw"}
	if got := strings.Join(args, "\x00"); got != strings.Join(want, "\x00") {
		t.Fatalf("mackerVolumeArgs(readOnly supported) = %#v, want %#v", args, want)
	}

	// A single file cannot be a read-only nullfs mount, so maclet must say so at
	// admission rather than letting Macker fail while mounting the workload.
	readOnlyFile := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("config", config, "File")}}}
	if _, err := mackerVolumeArgs(readOnlyFile, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "config", MountPath: "/etc/config.txt", ReadOnly: true}}}, true); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("readOnly File mount error = %v, want a not-a-directory rejection", err)
	}
	// The same manifest without readOnly stays a plain writable symlink.
	args, err = mackerVolumeArgs(readOnlyFile, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "config", MountPath: "/etc/config.txt"}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-v", config + ":/etc/config.txt"}; strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("writable File mount args = %#v, want %#v", args, want)
	}

	// A subPath of a Directory that resolves to a file is the same trap.
	if _, err := mackerVolumeArgs(pod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/file", SubPath: "file.txt", ReadOnly: true}}}, true); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("readOnly subPath-file error = %v, want a not-a-directory rejection", err)
	}
}

func TestVolumeArgRefusalsAreClassifiedAsManifestConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "html")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "config.txt")
	if err := os.WriteFile(file, []byte("config"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirPod := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("data", dir, "Directory")}}}
	filePod := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("config", file, "File")}}}
	// Nothing creates this path, so failing on it is the node's problem.
	missingPod := Pod{Spec: PodSpec{Volumes: []Volume{testHostPathVolume("gone", filepath.Join(root, "gone"), "Directory")}}}

	refusals := []struct {
		name      string
		pod       Pod
		mounts    []VolumeMount
		supported bool
	}{
		{"readOnly without runtime support", dirPod, []VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true}}, false},
		{"readOnly on a single file", filePod, []VolumeMount{{Name: "config", MountPath: "/etc/config.txt", ReadOnly: true}}, true},
		{"subPathExpr", dirPod, []VolumeMount{{Name: "data", MountPath: "/data", SubPathExpr: "$(POD_NAME)"}}, true},
		{"duplicate mount path", dirPod, []VolumeMount{{Name: "data", MountPath: "/data"}, {Name: "data", MountPath: "/data"}}, true},
		{"unknown volume", dirPod, []VolumeMount{{Name: "nope", MountPath: "/data"}}, true},
		{"relative mountPath", dirPod, []VolumeMount{{Name: "data", MountPath: "data"}}, true},
	}
	for _, tc := range refusals {
		_, err := mackerVolumeArgs(tc.pod, ContainerSpec{VolumeMounts: tc.mounts}, tc.supported)
		if err == nil {
			t.Fatalf("%s: expected a refusal", tc.name)
		}
		var refusal *manifestRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("%s: error = %v, want a manifestRefusal so it reports as CreateContainerConfigError", tc.name, err)
		}
		// The message is what an operator reads, so the wrapper must not swallow it.
		if strings.TrimSpace(refusal.Error()) == "" {
			t.Fatalf("%s: refusal message is empty", tc.name)
		}
	}

	_, err := mackerVolumeArgs(missingPod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "gone", MountPath: "/gone"}}}, true)
	if err == nil {
		t.Fatal("missing hostPath directory: expected an error")
	}
	var refusal *manifestRefusal
	if errors.As(err, &refusal) {
		t.Fatalf("missing hostPath directory should stay a runtime failure, got a refusal: %v", err)
	}
}

func TestMackerReadOnlyVolumeCapabilityProbe(t *testing.T) {
	help := "  -v value\n    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)\n"
	if !mackerHelpAdvertisesReadOnlyVolumes(help) {
		t.Fatal("read-only volume marker was not detected in Macker usage")
	}
	if mackerHelpAdvertisesReadOnlyVolumes("  -v value\n    \thost path and container path, HOST:CONTAINER (repeatable)\n") {
		t.Fatal("the writable-only usage must not look like read-only support")
	}
	root := t.TempDir()
	supported := filepath.Join(root, "macker-ro")
	unsupported := filepath.Join(root, "macker-plain")
	for path, usage := range map[string]string{supported: help, unsupported: "HOST:CONTAINER (repeatable)"} {
		script := "#!/bin/sh\necho '  -v value'\necho '" + strings.ReplaceAll(usage, "'", "'\\''") + "'\nexit 2\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if manager := newWorkloadManager(nil, supported, ""); !manager.supportsReadOnlyVolumes() {
		t.Fatal("supportsReadOnlyVolumes() = false for a Macker that advertises [:ro]")
	}
	manager := newWorkloadManager(nil, unsupported, "")
	if manager.supportsReadOnlyVolumes() {
		t.Fatal("supportsReadOnlyVolumes() = true for a Macker that does not advertise [:ro]")
	}
	// A negative answer is retried, because upgrading Macker must heal a node
	// that already probed the old binary.
	manager.mackerBinary = supported
	if manager.supportsReadOnlyVolumes() {
		t.Fatal("supportsReadOnlyVolumes() = false immediately after a negative probe")
	}
	manager.readOnlyVolumesProbedAt = time.Now().Add(-2 * mackerReadOnlyVolumeRetry)
	if !manager.supportsReadOnlyVolumes() {
		t.Fatal("supportsReadOnlyVolumes() did not re-probe an expired negative answer")
	}
}

// TestReadOnlyPlatformProbeRequiresNullfs pins the platform question: a node with
// no nullfs helper, no null.fs bundle, and no nullfs kernel feature cannot enforce
// a read-only hostPath mount, which is what the icarus node measures.
func TestReadOnlyPlatformProbeRequiresNullfs(t *testing.T) {
	absent := readOnlyPlatformProbe{exists: func(string) bool { return false }, kernelFeature: func(string) bool { return false }}
	if absent.hasNullfs() {
		t.Fatal("a node with no nullfs helper, bundle, or kernel feature reported read-only support")
	}
	for _, tc := range []struct {
		name   string
		probe  readOnlyPlatformProbe
		expect bool
	}{
		{"helper", readOnlyPlatformProbe{exists: func(path string) bool { return path == "/sbin/mount_nullfs" }, kernelFeature: func(string) bool { return false }}, true},
		{"legacy helper", readOnlyPlatformProbe{exists: func(path string) bool { return path == "/sbin/mount_null" }, kernelFeature: func(string) bool { return false }}, true},
		{"mount(8) bundle", readOnlyPlatformProbe{exists: func(path string) bool { return path == "/Library/Filesystems/null.fs" }, kernelFeature: func(string) bool { return false }}, true},
		{"kernel feature", readOnlyPlatformProbe{exists: func(string) bool { return false }, kernelFeature: func(name string) bool { return name == "vfs.generic.nullfs" }}, true},
	} {
		if got := tc.probe.hasNullfs(); got != tc.expect {
			t.Errorf("%s: hasNullfs() = %t, want %t", tc.name, got, tc.expect)
		}
	}
}

// TestReadOnlyVolumeCapabilityRefusesAdvertisedButUnenforceableNodes is the icarus
// case: Macker's usage text offers [:ro] because its argument grammar accepts it,
// while the OS has no nullfs. maclet must refuse at admission rather than forward a
// flag the kernel will reject.
func TestReadOnlyVolumeCapabilityRefusesAdvertisedButUnenforceableNodes(t *testing.T) {
	root := t.TempDir()
	supported := filepath.Join(root, "macker-ro")
	if err := os.WriteFile(supported, []byte("#!/bin/sh\necho '    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, supported, "")
	manager.readOnlyPlatform = &readOnlyPlatformProbe{exists: func(string) bool { return false }, kernelFeature: func(string) bool { return false }}
	if manager.supportsReadOnlyVolumes() {
		t.Fatal("maclet promised read-only volumes on a node whose OS cannot mount them")
	}
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "default", Name: "web", UID: "uid-1"}, Spec: PodSpec{Volumes: []Volume{{Name: "data", VolumeSource: VolumeSource{HostPath: &HostPathVolumeSource{Path: t.TempDir()}}}}}}
	container := ContainerSpec{VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/var/data", ReadOnly: true}}}
	if _, err := mackerVolumeArgs(pod, container, false); err == nil || !strings.Contains(err.Error(), "cannot enforce a read-only hostPath mount") {
		t.Fatalf("refusal did not name the enforcement gap: %v", err)
	}
	// A node that does have nullfs gets the flag forwarded, so this gate is not a
	// blanket refusal.
	manager.readOnlyPlatform = &readOnlyPlatformProbe{exists: func(path string) bool { return path == "/sbin/mount_nullfs" }, kernelFeature: func(string) bool { return false }}
	manager.readOnlyVolumes = nil
	if !manager.supportsReadOnlyVolumes() {
		t.Fatal("maclet refused read-only volumes on a node that has nullfs")
	}
	args, err := mackerVolumeArgs(pod, container, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || !strings.HasSuffix(args[1], "/var/data:ro") {
		t.Fatalf("volume args = %#v, want a :ro suffix", args)
	}
}

// TestNoteReadOnlyVolumeNodeRejection covers the second line of defence: a node that
// passes the presence probe and still refuses the mount, for example because the
// service user cannot sudo, must stop being handed doomed workloads.
func TestNoteReadOnlyVolumeNodeRejection(t *testing.T) {
	root := t.TempDir()
	supported := filepath.Join(root, "macker-ro")
	if err := os.WriteFile(supported, []byte("#!/bin/sh\necho '    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, supported, "")
	if !manager.supportsReadOnlyVolumes() {
		t.Fatal("baseline: advertised read-only support should be accepted off-Darwin")
	}
	// An unrelated launch failure is not a capability answer.
	manager.noteReadOnlyVolumeNodeRejection("error: configure container rootfs: token ____MACKER_PORT_1____ has no runtime value")
	if !manager.supportsReadOnlyVolumes() {
		t.Fatal("an unrelated launch failure downgraded read-only capability")
	}
	manager.noteReadOnlyVolumeNodeRejection("macker run ... : exit status 1 (error: no mount helper was found: tried /sbin/mount_nullfs, /sbin/mount and LookPath(mount); mount: /private/tmp/x failed with 72)")
	if manager.supportsReadOnlyVolumes() {
		t.Fatal("maclet kept promising read-only volumes after the node refused the mount")
	}
	// Installing a different Macker is exactly the event that should ask again.
	if err := os.WriteFile(supported, []byte("#!/bin/sh\necho '    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)'\necho '# the node gained nullfs in this build'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !manager.supportsReadOnlyVolumes() {
		t.Fatal("a Macker roll did not clear the learned node rejection")
	}
}

func TestMackerReportsReadOnlyMountFailure(t *testing.T) {
	for _, detail := range []string{
		"error: mount_nullfs was not found; it ships with macOS",
		"error: no mount helper was found: tried /sbin/mount_nullfs, /usr/sbin/mount_nullfs, then /sbin/mount",
		"mount: exec /Library/Filesystems/null.fs/Contents/Resources/mount_null for /private/tmp/mdst: No such file or directory",
		"error: mount -t null -o ro /tmp/a /tmp/b: exit status 71",
		"error: read-only volume mount failed: operation not permitted",
	} {
		if !mackerReportsReadOnlyMountFailure(detail) {
			t.Errorf("read-only mount failure not recognised: %q", detail)
		}
	}
	for _, detail := range []string{
		"error: configure container rootfs: token ____MACKER_PORT_1____ has no runtime value",
		"error: run docker.io/library/busybox:latest: image is not present in local storage",
		"error: mount_nfs failed: host unreachable",
		"exit status 1",
	} {
		if mackerReportsReadOnlyMountFailure(detail) {
			t.Errorf("not a read-only mount failure, but matched: %q", detail)
		}
	}
}

// TestReadOnlyVolumeCapabilityPrefersMackerVolumesVerdict keeps the two probes from
// disagreeing where the runtime has the better view: Macker knows about a vendored
// mount helper (MACKER_MOUNT_NULLFS) and about the mount path it actually takes,
// which maclet's look at /sbin and the kernel feature table cannot see.
func TestReadOnlyVolumeCapabilityPrefersMackerVolumesVerdict(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "macker-probing")
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"'volumes --format') echo 'RESULT'; exit 0;;\n" +
		"esac\n" +
		"echo '    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)'\n" +
		"exit 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	absentPlatform := &readOnlyPlatformProbe{exists: func(string) bool { return false }, kernelFeature: func(string) bool { return false }}
	supportedJSON := `{"read_only_supported": true, "mechanism": "helper", "reason": "MACKER_MOUNT_NULLFS override"}`
	refusedJSON := `{"read_only_supported": false, "mechanism": "none", "reason": "this macOS has no null filesystem bundle"}`
	for _, tc := range []struct {
		name    string
		verdict string
		expect  bool
	}{
		{"runtime accepts a helper maclet cannot see", supportedJSON, true},
		{"runtime refuses despite the marker", refusedJSON, false},
	} {
		if err := os.WriteFile(binary, []byte(strings.Replace(script, "RESULT", tc.verdict, 1)), 0o755); err != nil {
			t.Fatal(err)
		}
		manager := newWorkloadManager(nil, binary, "")
		manager.readOnlyPlatform = absentPlatform
		if got := manager.supportsReadOnlyVolumes(); got != tc.expect {
			t.Errorf("%s: supportsReadOnlyVolumes() = %t, want %t", tc.name, got, tc.expect)
		}
	}
	// A Macker without the `volumes` subcommand answers with usage text, which is not a
	// verdict: the help marker and the platform probe have to carry the decision.
	legacy := filepath.Join(root, "macker-legacy")
	if err := os.WriteFile(legacy, []byte("#!/bin/sh\necho '    \thost path and container path, HOST:CONTAINER[:ro] (repeatable)'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, legacy, "")
	manager.readOnlyPlatform = absentPlatform
	if manager.supportsReadOnlyVolumes() {
		t.Fatal("a legacy Macker on a platform without nullfs reported enforceable read-only volumes")
	}
}

func TestParseMackerReadOnlyVerdictIgnoresUsageText(t *testing.T) {
	if _, ok := parseMackerReadOnlyVerdict([]byte("usage:\n  macker volumes [--format json|table]\n")); ok {
		t.Fatal("usage text parsed as a capability verdict")
	}
	verdict, ok := parseMackerReadOnlyVerdict([]byte("read-only volumes: refused\n{\"read_only_supported\":false,\"mechanism\":\"none\"}\n"))
	if !ok || verdict.ReadOnlySupported {
		t.Fatalf("verdict = %+v ok=%t, want a refused answer", verdict, ok)
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
	// A Pending Pod must name its reason somewhere a client can branch on, and
	// that field is PodStatus.reason. The container waiting state stays inside
	// the kubelet vocabulary so standard tooling still recognises it.
	if status.Reason != "MacletUnsupportedPod" {
		t.Fatalf("Pod status reason = %q, want MacletUnsupportedPod", status.Reason)
	}
	if waiting := status.ContainerStatuses[0].State.Waiting; waiting == nil || waiting.Reason != "CreateContainerConfigError" {
		t.Fatalf("waiting status = %#v", status.ContainerStatuses[0].State.Waiting)
	}
	for _, condition := range status.Conditions {
		if (condition.Type == "Ready" || condition.Type == "ContainersReady") && (condition.Status != "False" || condition.Reason != "ContainersNotReady") {
			t.Fatalf("not-ready condition = %#v", condition)
		}
	}

	// A Pod maclet will never supervise is a manifest problem, not progress.
	// Before this it inherited the default ContainerCreating, which claims maclet
	// is still working on a Pod that is only ever going to sit Pending.
	unlabelled := desiredPodStatus(pod, "192.168.137.111", "Pending", "", "MacletMissingNativeLabel", unlabelledPendingMessage, false, 0)
	if unlabelled.Reason != "MacletMissingNativeLabel" {
		t.Fatalf("unlabelled Pod reason = %q", unlabelled.Reason)
	}
	if waiting := unlabelled.ContainerStatuses[0].State.Waiting; waiting == nil || waiting.Reason != "CreateContainerConfigError" {
		t.Fatalf("unlabelled Pod waiting status = %#v, want CreateContainerConfigError", unlabelled.ContainerStatuses[0].State.Waiting)
	}

	// A running workload keeps the canonical empty reason and no waiting state.
	running := desiredPodStatus(pod, "192.168.137.111", "Running", "10.42.8.3", "MacletWorkloadRunning", "running", true, 0)
	if running.Reason != "" || running.ContainerStatuses[0].State.Waiting != nil {
		t.Fatalf("running Pod = reason %q waiting %#v", running.Reason, running.ContainerStatuses[0].State.Waiting)
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
		if serveEvent(response, request) {
			return
		}
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

func TestExplainUnlabelledPodReportsOnce(t *testing.T) {
	var writes int32
	var events int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if serveEvent(response, request) {
			atomic.AddInt32(&events, 1)
			return
		}
		if request.Method != http.MethodPut || request.URL.Path != "/api/v1/namespaces/default/pods/web/status" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		atomic.AddInt32(&writes, 1)
		var payload struct {
			Metadata ObjectMeta `json:"metadata"`
			Status   PodStatus  `json:"status"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(Pod{ObjectMeta: payload.Metadata, Status: payload.Status})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, "", "192.168.137.111")
	pod := &Pod{
		ObjectMeta: ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1", ResourceVersion: "7"},
		Spec:       PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}},
		Status:     PodStatus{Phase: "Pending"},
	}
	manager.explainUnlabelledPod(context.Background(), client, pod)
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("status writes = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&events); got != 1 {
		t.Fatalf("Events filed = %d, want 1 so `kubectl describe pod` shows the node's own account", got)
	}
	if pod.Status.Message != unlabelledPendingMessage || string(pod.Status.Phase) != "Pending" {
		t.Fatalf("unlabelled Pod status = %q/%q", pod.Status.Phase, pod.Status.Message)
	}
	// maclet reconciles every few seconds, so an explained Pod must not churn.
	manager.explainUnlabelledPod(context.Background(), client, pod)
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("status writes after re-explaining = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&events); got != 1 {
		t.Fatalf("Events filed after re-explaining = %d, want the state said once", got)
	}
	// A Pod maclet never ran is not rewritten into Pending.
	terminal := &Pod{
		ObjectMeta: ObjectMeta{Name: "web", Namespace: "default", UID: "uid-2", ResourceVersion: "8"},
		Spec:       PodSpec{Containers: []ContainerSpec{{Name: "web", Image: "example/web:latest"}}},
		Status:     PodStatus{Phase: "Running"},
	}
	manager.explainUnlabelledPod(context.Background(), client, terminal)
	if got := atomic.LoadInt32(&writes); got != 1 {
		t.Fatalf("status writes for a Running Pod = %d, want 1", got)
	}
}
