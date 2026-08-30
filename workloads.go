package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	nativeWorkloadLabelKey   = "k8s-darwin.dev/native"
	nativeWorkloadLabelValue = "true"
	workloadRetryDelay       = 10 * time.Second
)

type managedWorkload struct {
	UID           string
	Namespace     string
	Name          string
	ContainerName string
	IP            string
	RestartCount  int32
	RetryAfter    time.Time
}

type mackerInspection struct {
	Status            string     `json:"status"`
	PID               int        `json:"pid"`
	WorkloadPID       int        `json:"workload_pid"`
	ExitCode          *int32     `json:"exit_code"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	TerminationSignal string     `json:"termination_signal"`
	TerminationReason string     `json:"termination_reason"`
}

type workloadJournalRecord struct {
	UID           string `json:"uid"`
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	ContainerName string `json:"containerName"`
	IP            string `json:"ip,omitempty"`
	RestartCount  int32  `json:"restartCount,omitempty"`
}

type workloadJournal struct {
	Version   int                     `json:"version"`
	Workloads []workloadJournalRecord `json:"workloads"`
}

type workloadManager struct {
	network      *DarwinNetworkHandle
	mackerBinary string
	nodeIP       string
	journalPath  string
	workloads    map[string]*managedWorkload
}

func newWorkloadManager(network *DarwinNetworkHandle, mackerBinary, nodeIP string) *workloadManager {
	return newWorkloadManagerWithState(network, mackerBinary, nodeIP, "")
}

func newWorkloadManagerWithState(network *DarwinNetworkHandle, mackerBinary, nodeIP, stateDir string) *workloadManager {
	journalPath := ""
	if stateDir != "" {
		journalPath = filepath.Join(stateDir, "workloads.json")
	}
	return &workloadManager{
		network:      network,
		mackerBinary: mackerBinary,
		nodeIP:       nodeIP,
		journalPath:  journalPath,
		workloads:    make(map[string]*managedWorkload),
	}
}

func (m *workloadManager) loadJournal() error {
	if m.journalPath == "" {
		return nil
	}
	body, err := os.ReadFile(m.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workload journal: %w", err)
	}
	var journal workloadJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return fmt.Errorf("decode workload journal: %w", err)
	}
	if journal.Version != 1 {
		return fmt.Errorf("unsupported workload journal version %d", journal.Version)
	}
	for _, record := range journal.Workloads {
		if record.UID == "" || record.ContainerName == "" {
			return errors.New("workload journal contains an incomplete record")
		}
		m.workloads[record.UID] = &managedWorkload{
			UID:           record.UID,
			Namespace:     record.Namespace,
			Name:          record.Name,
			ContainerName: record.ContainerName,
			IP:            record.IP,
			RestartCount:  record.RestartCount,
		}
	}
	return nil
}

func (m *workloadManager) persistJournal() error {
	if m.journalPath == "" {
		return nil
	}
	records := make([]workloadJournalRecord, 0, len(m.workloads))
	for _, workload := range m.workloads {
		records = append(records, workloadJournalRecord{
			UID: workload.UID, Namespace: workload.Namespace, Name: workload.Name,
			ContainerName: workload.ContainerName, IP: workload.IP, RestartCount: workload.RestartCount,
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UID < records[j].UID })
	body, err := json.MarshalIndent(workloadJournal{Version: 1, Workloads: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workload journal: %w", err)
	}
	if err := writePrivateFile(m.journalPath, append(body, '\n'), 0600); err != nil {
		return fmt.Errorf("write workload journal: %w", err)
	}
	return nil
}

func (m *workloadManager) mackerCommand(args ...string) (*exec.Cmd, error) {
	binary := m.mackerBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("macker")
		if err != nil {
			return nil, errors.New("macker executable was not found; set --macker-binary or add macker to PATH")
		}
	}
	return exec.Command(binary, args...), nil
}

func (m *workloadManager) mackerOutput(args ...string) ([]byte, error) {
	command, err := m.mackerCommand(args...)
	if err != nil {
		return nil, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("macker %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("macker %s: %w (%s)", strings.Join(args, " "), err, message)
	}
	return output, nil
}

func mackerContainerStatus(output, name string) (status string, found bool) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 && fields[0] == name {
			return fields[2], true
		}
	}
	return "", false
}

func (m *workloadManager) containerStatus(name string) (string, bool, error) {
	output, err := m.mackerOutput("ps", "--all")
	if err != nil {
		return "", false, err
	}
	status, found := mackerContainerStatus(string(output), name)
	return status, found, nil
}

func (m *workloadManager) containerInspection(name string) (*mackerInspection, bool, error) {
	output, err := m.mackerOutput("inspect", "--format", "json", name)
	if err != nil {
		// Keep compatibility with Macker versions predating inspect. The
		// lifecycle reconciler can still use ps status, but cannot report an
		// exit code until the newer Macker binary is installed.
		return nil, false, nil
	}
	var inspection mackerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		return nil, true, fmt.Errorf("decode Macker inspect output for %s: %w", name, err)
	}
	return &inspection, true, nil
}

func workloadContainerName(pod Pod) string {
	name := "maclet-" + pod.Metadata.Namespace + "-" + pod.Metadata.Name
	if pod.Metadata.UID != "" {
		name += "-" + pod.Metadata.UID
	}
	name = strings.ToLower(name)
	var builder strings.Builder
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	name = strings.Trim(builder.String(), "-._")
	if len(name) > 120 {
		name = name[:120]
		name = strings.TrimRight(name, "-._")
	}
	return name
}

func podRestartPolicy(pod Pod) string {
	if pod.Spec.RestartPolicy == "" {
		return "Always"
	}
	return pod.Spec.RestartPolicy
}

func ensureHostPath(path string, volumeType string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("hostPath %q must be an absolute path", path)
	}
	path = filepath.Clean(path)
	if strings.Contains(path, ":") {
		return "", fmt.Errorf("hostPath %q cannot be passed to Macker because it contains ':'", path)
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		switch volumeType {
		case "DirectoryOrCreate":
			if err := os.MkdirAll(path, 0o755); err != nil {
				return "", fmt.Errorf("create hostPath directory %q: %w", path, err)
			}
			info, err = os.Stat(path)
		case "FileOrCreate":
			file, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if createErr != nil {
				return "", fmt.Errorf("create hostPath file %q: %w", path, createErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				return "", fmt.Errorf("close hostPath file %q: %w", path, closeErr)
			}
			info, err = os.Stat(path)
		default:
			return "", fmt.Errorf("hostPath %q does not exist", path)
		}
	}
	if err != nil {
		return "", fmt.Errorf("inspect hostPath %q: %w", path, err)
	}
	switch volumeType {
	case "":
		// An empty hostPath type preserves Macker's existing requirement that
		// the source exists without imposing a file-type check.
	case "Directory", "DirectoryOrCreate":
		if !info.IsDir() {
			return "", fmt.Errorf("hostPath %q is not a directory", path)
		}
	case "File", "FileOrCreate":
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("hostPath %q is not a regular file", path)
		}
	case "Socket":
		if info.Mode()&os.ModeSocket == 0 {
			return "", fmt.Errorf("hostPath %q is not a Unix socket", path)
		}
	case "CharDevice":
		if info.Mode()&os.ModeCharDevice == 0 {
			return "", fmt.Errorf("hostPath %q is not a character device", path)
		}
	case "BlockDevice":
		if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("hostPath %q is not a block device", path)
		}
	default:
		return "", fmt.Errorf("hostPath type %q is not supported; use Directory, DirectoryOrCreate, File, or FileOrCreate", volumeType)
	}
	return path, nil
}

func resolveVolumeSubPath(hostPath, subPath string) (string, error) {
	if subPath == "" || subPath == "." {
		return hostPath, nil
	}
	if filepath.IsAbs(subPath) {
		return "", fmt.Errorf("volume subPath %q must be relative", subPath)
	}
	clean := filepath.Clean(subPath)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume subPath %q escapes the hostPath", subPath)
	}
	target := filepath.Join(hostPath, clean)
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("inspect volume subPath %q: %w", subPath, err)
	}
	resolvedHost, err := filepath.EvalSymlinks(hostPath)
	if err != nil {
		return "", fmt.Errorf("resolve hostPath %q: %w", hostPath, err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve volume subPath %q: %w", subPath, err)
	}
	relative, err := filepath.Rel(resolvedHost, resolvedTarget)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume subPath %q escapes the hostPath through a symlink", subPath)
	}
	return target, nil
}

func mackerVolumeArgs(pod Pod, container ContainerSpec) ([]string, error) {
	volumes := make(map[string]string, len(pod.Spec.Volumes))
	volumeTypes := make(map[string]string, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "" {
			continue
		}
		if _, exists := volumeTypes[volume.Name]; exists {
			return nil, fmt.Errorf("Pod volume %q is declared more than once", volume.Name)
		}
		if volume.HostPath == nil {
			volumeTypes[volume.Name] = ""
			continue
		}
		hostPath, err := ensureHostPath(volume.HostPath.Path, volume.HostPath.Type)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		volumes[volume.Name] = hostPath
		volumeTypes[volume.Name] = volume.HostPath.Type
	}
	args := make([]string, 0, len(container.VolumeMounts)*2)
	seenMountPaths := make(map[string]struct{}, len(container.VolumeMounts))
	for _, mount := range container.VolumeMounts {
		// Kubernetes injects this projected service-account mount into Pods by
		// default. Native workloads do not receive that Linux token volume.
		if mount.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
			continue
		}
		if mount.Name == "" {
			return nil, fmt.Errorf("volume mount at %q has no volume name", mount.MountPath)
		}
		if mount.MountPath == "" || !filepath.IsAbs(mount.MountPath) {
			return nil, fmt.Errorf("volume mount %q must use an absolute mountPath", mount.MountPath)
		}
		mountPath := filepath.Clean(mount.MountPath)
		if _, exists := seenMountPaths[mountPath]; exists {
			return nil, fmt.Errorf("volume mount path %q is declared more than once", mountPath)
		}
		seenMountPaths[mountPath] = struct{}{}
		if mount.ReadOnly {
			return nil, fmt.Errorf("volume mount %q requests readOnly, but Macker hostPath volumes are writable live symlinks", mountPath)
		}
		if mount.SubPathExpr != "" {
			return nil, fmt.Errorf("volume mount %q uses subPathExpr, which maclet does not support", mountPath)
		}
		hostPath, exists := volumes[mount.Name]
		if !exists {
			if _, declared := volumeTypes[mount.Name]; declared {
				return nil, fmt.Errorf("volume %q is not a supported hostPath volume", mount.Name)
			}
			return nil, fmt.Errorf("volume mount %q references unknown volume %q", mountPath, mount.Name)
		}
		resolvedPath, err := resolveVolumeSubPath(hostPath, mount.SubPath)
		if err != nil {
			return nil, fmt.Errorf("volume mount %q: %w", mountPath, err)
		}
		if strings.Contains(resolvedPath, ":") || strings.Contains(mountPath, ":") {
			return nil, fmt.Errorf("volume mount %q cannot be passed to Macker because its path contains ':'", mountPath)
		}
		args = append(args, "-v", resolvedPath+":"+mountPath)
	}
	return args, nil
}

func (m *workloadManager) runArgs(pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	if container.Image == "" {
		return nil, errors.New("container image is empty")
	}
	if container.WorkingDir != "" {
		return nil, fmt.Errorf("container %q sets workingDir %q, which Macker does not support yet", container.Name, container.WorkingDir)
	}
	volumeArgs, err := mackerVolumeArgs(pod, container)
	if err != nil {
		return nil, fmt.Errorf("container %q: %w", container.Name, err)
	}
	if m.network == nil || m.network.Interface == "" || m.network.Gateway == "" {
		return nil, errors.New("Darwin workload networking is not available; join with VXLAN enabled")
	}
	bridgeIP := m.network.Gateway
	// The bridge's first usable address is the network address immediately
	// before the synthetic gateway. This is what Macker exposes as the
	// optional host-side context for an externally attached network.
	if gateway := net.ParseIP(m.network.Gateway); gateway != nil {
		value := gateway.To4()
		if value != nil {
			bridge := make(net.IP, net.IPv4len)
			copy(bridge, value)
			bridge[3]--
			bridgeIP = bridge.String()
		}
	}
	args := []string{
		"run", "--detach",
		"--net=external",
		"--interface", m.network.Interface,
		"--ip", managed.IP,
		"--host-interface", m.network.Interface,
		"--host-ip", bridgeIP,
		"--name", managed.ContainerName,
	}
	args = append(args, volumeArgs...)
	for _, env := range container.Env {
		if env.Name == "" {
			return nil, errors.New("container environment variable has an empty name")
		}
		if env.ValueFrom != nil {
			return nil, fmt.Errorf("container environment variable %q uses valueFrom, which maclet does not support yet", env.Name)
		}
		args = append(args, "--env", env.Name+"="+env.Value)
	}
	for index, port := range container.Ports {
		if port.HostPort != 0 {
			return nil, fmt.Errorf("container port %d requests hostPort %d; maclet does not support host-port mapping yet", port.ContainerPort, port.HostPort)
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return nil, fmt.Errorf("container port %d is outside the valid range", port.ContainerPort)
		}
		protocol := strings.ToUpper(port.Protocol)
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" && protocol != "UDP" {
			return nil, fmt.Errorf("container port %d uses unsupported protocol %q", port.ContainerPort, port.Protocol)
		}
		// Macker's config-token expansion consumes MACKER_PORT_N. These are
		// workload environment values, not host PF publications; the Pod IP
		// itself is already directly reachable through the VXLAN bridge.
		args = append(args, "--env", fmt.Sprintf("MACKER_PORT_%d=%d", index+1, port.ContainerPort))
	}
	if len(container.Command) > 0 {
		args = append(args, "--entrypoint", container.Command[0])
	}
	args = append(args, container.Image)
	overrideLength := len(container.Args)
	if len(container.Command) > 1 {
		overrideLength += len(container.Command) - 1
	}
	commandOverride := make([]string, 0, overrideLength)
	if len(container.Command) > 1 {
		commandOverride = append(commandOverride, container.Command[1:]...)
	}
	commandOverride = append(commandOverride, container.Args...)
	if len(commandOverride) > 0 {
		args = append(args, "--")
		args = append(args, commandOverride...)
	}
	return args, nil
}

func podStatusPath(pod Pod) string {
	return "/api/v1/namespaces/" + url.PathEscape(pod.Metadata.Namespace) + "/pods/" + url.PathEscape(pod.Metadata.Name) + "/status"
}

func podPath(pod Pod) string {
	return "/api/v1/namespaces/" + url.PathEscape(pod.Metadata.Namespace) + "/pods/" + url.PathEscape(pod.Metadata.Name)
}

func setPodCondition(conditions []PodCondition, condition PodCondition) []PodCondition {
	updated := make([]PodCondition, 0, len(conditions)+1)
	found := false
	for _, existing := range conditions {
		if existing.Type == condition.Type {
			if !found {
				updated = append(updated, condition)
				found = true
			}
			continue
		}
		updated = append(updated, existing)
	}
	if !found {
		updated = append(updated, condition)
	}
	return updated
}

func desiredPodStatus(pod Pod, nodeIP, phase, ip, reason, message string, running bool, restartCount int32, termination ...*mackerInspection) PodStatus {
	status := pod.Status
	var inspection *mackerInspection
	if len(termination) > 0 {
		inspection = termination[0]
	}
	status.Phase = phase
	status.PodIP = ip
	status.PodIPs = nil
	if ip != "" {
		status.PodIPs = []PodIP{{IP: ip}}
	}
	status.HostIP = nodeIP
	status.Reason = reason
	status.Message = message
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	ready := running && phase == "Running"
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "PodScheduled", Status: "True", LastTransitionTime: stamp, Reason: "MacletAssigned", Message: "maclet assigned this trusted native workload"})
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "Initialized", Status: "True", LastTransitionTime: stamp, Reason: "MacletInitialized", Message: "maclet initialized the native workload"})
	readyReason := "MacletWorkloadNotRunning"
	readyMessage := "the Macker workload is not running"
	if ready {
		readyReason = "MacletWorkloadRunning"
		readyMessage = "the Macker workload is running"
	}
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "Ready", Status: map[bool]string{true: "True", false: "False"}[ready], LastTransitionTime: stamp, Reason: readyReason, Message: readyMessage})
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "ContainersReady", Status: map[bool]string{true: "True", false: "False"}[ready], LastTransitionTime: stamp, Reason: readyReason, Message: readyMessage})
	container := ContainerStatus{Name: "", Ready: ready, RestartCount: restartCount, State: ContainerState{}}
	if len(pod.Spec.Containers) > 0 {
		container.Name = pod.Spec.Containers[0].Name
		container.Image = pod.Spec.Containers[0].Image
	}
	switch {
	case running:
		container.State.Running = &ContainerStateRunning{StartedAt: stamp}
	case phase == "Succeeded" || phase == "Failed":
		terminated := &ContainerStateTerminated{Reason: reason, Message: message, FinishedAt: stamp}
		if inspection != nil {
			if inspection.ExitCode != nil {
				terminated.ExitCode = *inspection.ExitCode
			}
			if inspection.StartedAt != nil {
				terminated.StartedAt = inspection.StartedAt.UTC().Format(time.RFC3339Nano)
			}
			if inspection.FinishedAt != nil {
				terminated.FinishedAt = inspection.FinishedAt.UTC().Format(time.RFC3339Nano)
			}
			if inspection.TerminationReason != "" || inspection.TerminationSignal != "" {
				terminationDetail := inspection.TerminationReason
				if inspection.TerminationSignal != "" {
					if terminationDetail != "" {
						terminationDetail += "; "
					}
					terminationDetail += "signal=" + inspection.TerminationSignal
				}
				if terminationDetail != "" {
					terminated.Message += "; Macker termination: " + terminationDetail
				}
			}
		}
		container.State.Terminated = terminated
	default:
		container.State.Waiting = &ContainerStateWaiting{Reason: reason, Message: message}
	}
	status.ContainerStatuses = []ContainerStatus{container}
	return status
}

func updatePodStatus(ctx context.Context, client *APIClient, pod *Pod, desired PodStatus) (*Pod, error) {
	current := pod
	for attempt := 0; attempt < 5; attempt++ {
		payload := map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":            current.Metadata.Name,
				"namespace":       current.Metadata.Namespace,
				"uid":             current.Metadata.UID,
				"resourceVersion": current.Metadata.ResourceVersion,
				// NodeRestriction compares the full object metadata during a
				// status update. Preserve the labels/annotations owned by the
				// user and admission controllers instead of submitting them as
				// an empty metadata map.
				"labels":      current.Metadata.Labels,
				"annotations": current.Metadata.Annotations,
			},
			"status": desired,
		}
		body, err := client.putJSON(ctx, podStatusPath(*current), payload)
		if err == nil {
			var updated Pod
			if decodeErr := json.Unmarshal(body, &updated); decodeErr != nil {
				return nil, fmt.Errorf("decode Pod status response: %w", decodeErr)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(err, &conflict) || conflict.Code != 409 || attempt == 4 {
			return nil, fmt.Errorf("update Pod %s/%s status: %w", current.Metadata.Namespace, current.Metadata.Name, err)
		}
		body, getErr := client.get(ctx, podPath(*current))
		if getErr != nil {
			return nil, fmt.Errorf("refresh Pod after status conflict: %w", getErr)
		}
		var refreshed Pod
		if getErr := json.Unmarshal(body, &refreshed); getErr != nil {
			return nil, fmt.Errorf("decode Pod after status conflict: %w", getErr)
		}
		current = &refreshed
		desired = desiredPodStatus(*current, desired.HostIP, desired.Phase, desired.PodIP, desired.Reason, desired.Message, desired.Phase == "Running", firstContainerRestartCount(desired))
	}
	return nil, errors.New("Pod status update retry limit exceeded")
}

func firstContainerRestartCount(status PodStatus) int32 {
	if len(status.ContainerStatuses) == 0 {
		return 0
	}
	return status.ContainerStatuses[0].RestartCount
}

func podHasNativeLabel(pod Pod) bool {
	return pod.Metadata.Labels[nativeWorkloadLabelKey] == nativeWorkloadLabelValue
}

func (m *workloadManager) updateStatus(ctx context.Context, client *APIClient, pod *Pod, phase, ip, reason, message string, running bool, restartCount int32, termination ...*mackerInspection) error {
	desired := desiredPodStatus(*pod, m.nodeIP, phase, ip, reason, message, running, restartCount, termination...)
	updated, err := updatePodStatus(ctx, client, pod, desired)
	if err != nil {
		return err
	}
	*pod = *updated
	return nil
}

func (m *workloadManager) stopContainer(workload *managedWorkload) error {
	if workload == nil || workload.ContainerName == "" {
		return nil
	}
	var cleanupErrors []error
	if _, err := m.mackerOutput("stop", workload.ContainerName); err != nil && !ignorableMackerCleanupError(err) {
		cleanupErrors = append(cleanupErrors, err)
	}
	if _, err := m.mackerOutput("rm", "--force", workload.ContainerName); err != nil && !ignorableMackerCleanupError(err) {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func ignorableMackerCleanupError(err error) bool {
	if err == nil {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "was not found") || strings.Contains(message, "not a detached workload")
}

func (m *workloadManager) removeWorkload(workload *managedWorkload) error {
	if workload == nil {
		return nil
	}
	var cleanupErrors []error
	if err := m.stopContainer(workload); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if workload.IP != "" {
		if err := m.network.removeWorkloadIP(workload.IP); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func listAssignedPods(ctx context.Context, client *APIClient, nodeName string) ([]Pod, error) {
	query := url.Values{"fieldSelector": []string{"spec.nodeName=" + nodeName}}
	body, err := client.get(ctx, "/api/v1/pods?"+query.Encode())
	if err != nil {
		return nil, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, fmt.Errorf("decode assigned PodList: %w", err)
	}
	return pods.Items, nil
}

func (m *workloadManager) reconcile(ctx context.Context, client *APIClient, pods []Pod) error {
	if m.network == nil {
		return nil
	}
	used := make(map[string]bool)
	for _, pod := range pods {
		if pod.Status.PodIP != "" {
			used[pod.Status.PodIP] = true
		}
	}
	seen := make(map[string]bool)
	var reconcileErrors []error
	for index := range pods {
		pod := &pods[index]
		uid := pod.Metadata.UID
		if uid == "" {
			uid = pod.Metadata.Namespace + "/" + pod.Metadata.Name
		}
		if pod.Metadata.DeletionTimestamp != "" {
			seen[uid] = true
			workload := m.workloads[uid]
			if workload == nil {
				workload = &managedWorkload{
					UID:           uid,
					Namespace:     pod.Metadata.Namespace,
					Name:          pod.Metadata.Name,
					ContainerName: workloadContainerName(*pod),
					IP:            pod.Status.PodIP,
				}
			}
			if err := m.removeWorkload(workload); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("remove deleted Pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
				m.workloads[uid] = workload
				continue
			}
			delete(m.workloads, uid)
			if err := m.persistJournal(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist deleted Pod %s/%s cleanup: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			}
			continue
		}
		if !podHasNativeLabel(*pod) {
			continue
		}
		seen[uid] = true
		if len(pod.Spec.Containers) != 1 {
			err := fmt.Errorf("Pod %s/%s must declare exactly one container; maclet does not support sidecars yet", pod.Metadata.Namespace, pod.Metadata.Name)
			if statusErr := m.updateStatus(ctx, client, pod, "Failed", pod.Status.PodIP, "MacletUnsupportedPod", err.Error(), false, 0); statusErr != nil {
				reconcileErrors = append(reconcileErrors, statusErr)
			}
			continue
		}
		managed := m.workloads[uid]
		journalChanged := false
		if managed == nil {
			managed = &managedWorkload{UID: uid, ContainerName: workloadContainerName(*pod)}
			if existing := pod.Status.ContainerStatuses; len(existing) > 0 {
				managed.RestartCount = existing[0].RestartCount
			}
			m.workloads[uid] = managed
			journalChanged = true
		}
		if managed.Namespace != pod.Metadata.Namespace || managed.Name != pod.Metadata.Name {
			managed.Namespace = pod.Metadata.Namespace
			managed.Name = pod.Metadata.Name
			journalChanged = true
		}
		if journalChanged {
			if err := m.persistJournal(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
		}
		ip := pod.Status.PodIP
		if ip == "" {
			allocated, err := m.network.firstAvailableWorkloadIP(used)
			if err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("allocate address for Pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
				_ = m.updateStatus(ctx, client, pod, "Pending", "", "MacletAddressAllocationFailed", err.Error(), false, managed.RestartCount)
				continue
			}
			ip = allocated
			used[ip] = true
		}
		if err := m.network.validateWorkloadAddress(ip); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("Pod %s/%s has invalid PodIP %s: %w", pod.Metadata.Namespace, pod.Metadata.Name, ip, err))
			_ = m.updateStatus(ctx, client, pod, "Failed", ip, "MacletInvalidPodIP", err.Error(), false, managed.RestartCount)
			continue
		}
		if managed.IP != "" && managed.IP != ip {
			if err := m.network.removeWorkloadIP(managed.IP); err != nil {
				reconcileErrors = append(reconcileErrors, err)
				continue
			}
		}
		if managed.IP != ip {
			managed.IP = ip
			if err := m.persistJournal(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s address: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
		}
		if err := m.network.addWorkloadIP(ip); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("add address for Pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletNetworkSetupFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if (pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed") && podRestartPolicy(*pod) != "Always" {
			continue
		}
		if !managed.RetryAfter.IsZero() && time.Now().Before(managed.RetryAfter) {
			continue
		}
		status, found, err := m.containerStatus(managed.ContainerName)
		if err != nil {
			reconcileErrors = append(reconcileErrors, err)
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerUnavailable", err.Error(), false, managed.RestartCount)
			continue
		}
		if found && status == "running" {
			managed.RetryAfter = time.Time{}
			if err := m.updateStatus(ctx, client, pod, "Running", ip, "MacletWorkloadRunning", "Macker is running the trusted native workload", true, managed.RestartCount); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
			continue
		}
		if found && status != "running" {
			inspection, inspectionAvailable, inspectErr := m.containerInspection(managed.ContainerName)
			if inspectErr != nil {
				reconcileErrors = append(reconcileErrors, inspectErr)
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
			if !inspectionAvailable {
				inspection = nil
			}
			switch podRestartPolicy(*pod) {
			case "Never", "OnFailure":
				phase := "Succeeded"
				reason := "MacletWorkloadCompleted"
				message := "Macker stopped the workload"
				if podRestartPolicy(*pod) == "OnFailure" {
					phase = "Failed"
					reason = "MacletWorkloadExited"
					message = "Macker workload exited"
				}
				if inspection != nil && inspection.ExitCode != nil {
					if *inspection.ExitCode != 0 {
						phase = "Failed"
						reason = "MacletWorkloadFailed"
						message = fmt.Sprintf("Macker workload exited with code %d", *inspection.ExitCode)
					} else {
						phase = "Succeeded"
						reason = "MacletWorkloadCompleted"
						message = "Macker workload exited successfully"
					}
				} else if inspection == nil {
					if podRestartPolicy(*pod) == "Never" {
						message = "Macker stopped the workload; exit code is not available through the current Macker CLI"
					} else {
						message = "Macker workload exited; exit code is not available through the current Macker CLI"
					}
				}
				_ = m.stopContainer(managed)
				if err := m.updateStatus(ctx, client, pod, phase, ip, reason, message, false, managed.RestartCount, inspection); err != nil {
					reconcileErrors = append(reconcileErrors, err)
				}
				continue
			default:
				if err := m.stopContainer(managed); err != nil {
					reconcileErrors = append(reconcileErrors, err)
					managed.RetryAfter = time.Now().Add(workloadRetryDelay)
					continue
				}
				managed.RestartCount++
				if err := m.persistJournal(); err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s restart: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
					managed.RetryAfter = time.Now().Add(workloadRetryDelay)
					continue
				}
			}
		}
		args, err := m.runArgs(*pod, pod.Spec.Containers[0], managed)
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("prepare Macker workload %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Failed", ip, "MacletMackerLaunchFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if _, err := m.mackerOutput(args...); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("start Macker workload %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerLaunchFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if err := m.persistJournal(); err != nil {
			if cleanupErr := m.removeWorkload(managed); cleanupErr != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w (cleanup: %v)", pod.Metadata.Namespace, pod.Metadata.Name, err, cleanupErr))
			} else {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			}
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletOwnershipPersistFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		managed.RetryAfter = time.Time{}
		if err := m.updateStatus(ctx, client, pod, "Running", ip, "MacletWorkloadRunning", "Macker started the trusted native workload", true, managed.RestartCount); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	journalChanged := false
	for uid, workload := range m.workloads {
		if seen[uid] {
			continue
		}
		if err := m.removeWorkload(workload); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("remove stale maclet workload %s: %w", workload.ContainerName, err))
			continue
		}
		delete(m.workloads, uid)
		journalChanged = true
	}
	if journalChanged {
		if err := m.persistJournal(); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("persist stale workload cleanup: %w", err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func validateWorkloadAddress(cidr, ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return errors.New("address is not IPv4")
	}
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	if !network.Contains(parsed) {
		return fmt.Errorf("address is outside PodCIDR %s", cidr)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return errors.New("PodCIDR has no usable workload addresses")
	}
	value := uint32(0)
	for _, octet := range parsed.To4() {
		value = (value << 8) | uint32(octet)
	}
	networkValue := uint32(0)
	for _, octet := range network.IP.To4() {
		networkValue = (networkValue << 8) | uint32(octet)
	}
	hostBits := bits - prefix
	offset := value - networkValue
	broadcastOffset := uint32(1<<hostBits) - 1
	if offset < 3 || offset >= broadcastOffset {
		return errors.New("address is reserved or is the PodCIDR broadcast address")
	}
	return nil
}

func (h *DarwinNetworkHandle) validateWorkloadAddress(ip string) error {
	if err := validateWorkloadAddress(h.PodCIDR, ip); err != nil {
		return err
	}
	parsed := net.ParseIP(ip).To4()
	_, network, _ := net.ParseCIDR(h.PodCIDR)
	if h.isReservedWorkloadIP(parsed, network.IP) {
		return errors.New("address is reserved by maclet")
	}
	return nil
}

func (m *workloadManager) cleanup() error {
	var cleanupErrors []error
	journalChanged := false
	for uid, workload := range m.workloads {
		if err := m.removeWorkload(workload); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup workload %s: %w", uid, err))
			continue
		}
		delete(m.workloads, uid)
		journalChanged = true
	}
	if journalChanged || len(m.workloads) == 0 {
		if err := m.persistJournal(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}
