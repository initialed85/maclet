package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func firstPodContainerName(pod Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Name
}

func workloadContainerName(pod Pod) string {
	name := "maclet-" + pod.ObjectMeta.Namespace + "-" + pod.ObjectMeta.Name
	if pod.ObjectMeta.UID != "" {
		name += "-" + string(pod.ObjectMeta.UID)
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
	return string(pod.Spec.RestartPolicy)
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

// mackerReadOnlyVolumeMarker is the text Macker's own usage output must contain
// for maclet to send a third `ro` field in -v. Macker maps volumes with a live
// host symlink, so `:ro` is only accepted by binaries that can enforce it, and
// maclet never sends it to one that cannot.
const mackerReadOnlyVolumeMarker = "HOST:CONTAINER[:ro]"

func mackerHelpAdvertisesReadOnlyVolumes(help string) bool {
	return strings.Contains(help, mackerReadOnlyVolumeMarker)
}

// mackerReadOnlyVerdict is what `macker volumes [--probe] --format json` reports
// about the node it runs on. The runtime owns the mount path, including a vendored
// helper pointed at with MACKER_MOUNT_NULLFS, so where this answers, it wins over
// maclet's own look at the platform.
type mackerReadOnlyVerdict struct {
	ReadOnlySupported bool   `json:"read_only_supported"`
	Mechanism         string `json:"mechanism"`
	Reason            string `json:"reason"`
	// Probed and ProbeStatus exist from Macker f61c2bb / 52338ec. ProbeStatus is the
	// machine-readable verdict, and maclet branches on it rather than reading the
	// reason sentence, which is prose aimed at whoever opens `kubectl describe pod`.
	Probed      bool   `json:"probed"`
	ProbeStatus string `json:"probe_status"`
	// Guidance is Macker's node-operator checklist. It is long and technical, so
	// maclet logs it and keeps it out of the Pod's status: the answer a Pod needs is
	// `reason`, and a 900-character mount calibration essay is not decision-shaped.
	Guidance string `json:"guidance"`
	// Probe carries the same verdict with the mount lifecycle beside it. Macker reports
	// the status in both places -- `probe_status` for a machine that reads one field,
	// `status` inside `probe` next to the mount facts -- and the lifecycle detail only
	// exists in the nested record.
	Probe struct {
		Status string `json:"status"`
		// UnmountError is non-empty when the probe created a mount on this node and could
		// not take it off. It matters on verdicts that already refuse -- `write_accepted`
		// is a mount that stayed writable *and* may still be sitting on the node -- so it
		// is read whatever the status says, and it is node health rather than Pod advice.
		UnmountError    string `json:"unmount_error"`
		UnmountAttempts int    `json:"unmount_attempts"`
	} `json:"probe"`
}

// probeStatus reads Macker's machine-readable verdict from whichever record carries it.
func (v mackerReadOnlyVerdict) probeStatus() string {
	if status := strings.TrimSpace(v.ProbeStatus); status != "" {
		return status
	}
	return strings.TrimSpace(v.Probe.Status)
}

// probeUnmountFailure reports a probe mount that Macker could not release, so maclet can
// say so on the node even when the verdict was already a refusal.
func (v mackerReadOnlyVerdict) probeUnmountFailure() (string, int) {
	return strings.TrimSpace(v.Probe.UnmountError), v.Probe.UnmountAttempts
}

// logMackerProbeUnmountFailure writes the node-health half of a probe answer: a mount the
// runtime created and could not remove. Every such status already refuses :ro, so maclet is
// not changing its decision here -- it is reporting a leftover mount that a later probe, a
// later Pod, or whoever inspects the node will otherwise have to rediscover.
func (m *workloadManager) logMackerProbeUnmountFailure(verdict mackerReadOnlyVerdict) {
	unmountError, attempts := verdict.probeUnmountFailure()
	if unmountError == "" {
		return
	}
	log.Printf("macker read-only volume probe left a mount on this node: %s (release attempted %d times, probe status %q) -- ask the node owner to release it before the next probe is read as this node's answer", unmountError, attempts, verdict.probeStatus())
}

// Macker's probe outcomes, from `macker volumes --probe --format json`. The probe is
// the strongest evidence available: it attempts a real temporary read-only mount and
// a write inside it, so `enforced` cannot be produced by a helper merely being present.
const (
	mackerProbeEnforced   = "enforced"
	mackerProbePrivilege  = "mount_not_attempted_privilege"
	mackerProbeNotRequest = "not_requested"
	// Anything else -- not_attempted_os, helper_unavailable, mount_refused,
	// write_accepted, unmount_failed -- is the kernel or the runtime answering no, and
	// maclet refuses :ro on that answer.
)

// parseMackerReadOnlyVerdict reads the JSON object out of a `macker volumes` answer.
// A Macker that predates the subcommand prints usage text instead, which parses to
// nothing and sends the caller back to the help marker and the platform probe.
func parseMackerReadOnlyVerdict(output []byte) (mackerReadOnlyVerdict, bool) {
	start := strings.Index(string(output), "{")
	if start < 0 {
		return mackerReadOnlyVerdict{}, false
	}
	var verdict mackerReadOnlyVerdict
	if err := json.Unmarshal(output[start:], &verdict); err != nil {
		return mackerReadOnlyVerdict{}, false
	}
	// Macker reports the verdict under its own name; anything without that key is not
	// an answer, so it must not be read as "unsupported".
	if !strings.Contains(string(output[start:]), "read_only_supported") {
		return mackerReadOnlyVerdict{}, false
	}
	return verdict, true
}

// mackerReportsReadOnlyMountFailure recognises Macker saying it could not perform
// a read-only volume mount, as opposed to the workload failing for an unrelated
// reason. These are the node's own words: an older build reports the helper by
// name, the current build lists the forms it tried, and mount(8) contributes its
// own rejection text. Anything else is a launch failure we should not read as a
// capability answer.
func mackerReportsReadOnlyMountFailure(detail string) bool {
	lower := strings.ToLower(detail)
	for _, signal := range []string{
		"nullfs",
		"no mount helper was found",
		"mount -t null",
		"read-only volume",
		"mount_null",
	} {
		if strings.Contains(lower, signal) {
			return true
		}
	}
	return false
}

// readOnlyPlatformProbe answers whether this operating system can make a
// read-only bind mount at all, independently of which Macker is installed. It
// exists because Macker advertises `HOST:CONTAINER[:ro]` from its argument
// grammar, which describes what the binary is able to ask for rather than what
// this kernel is able to do. Measured on the icarus node (macOS 26.6.2, arm64): no
// /sbin/mount_nullfs, no mount_null helper in /sbin at all, no null.fs bundle for
// mount(8) to exec (`mount -t null` fails with error 72). The kernel half is settled by
// macker's calibrated mount(2) probe on that node: the registered type name is `nullfs`
// and it answers EPERM even as root, where the Linux-style name `null` answers ENODEV (19)
// exactly as a made-up name does. That matches macker's reading of xnu-12377.121.6, where
// bsd/miscfs/nullfs is built in and nullfs_mount() is gated on the private entitlement
// com.apple.private.nullfs_allow with no superuser fallback -- so the refusal is a gate on
// who holds the entitlement rather than on where a helper came from: a caller that held
// com.apple.private.nullfs_allow would be allowed to mount, and no helper obtainable on
// this node holds it, vendored or otherwise, so neither root nor a `MACKER_MOUNT_NULLFS`
// override changes the answer here. The `vfs.generic.nullfs` sysctl is read for wording only and
// never grants (see hasNullfs): `vfs.generic` lists tunables, not registered filesystems
// (it omits devfs, which demonstrably mounts), so its absence settles nothing and its
// presence would not make the mount enforceable. Everything ends the same place: no
// Macker build can enforce a read-only hostPath mount on this OS, so maclet refuses it at
// admission. The probe checks presence rather than mounting, because a real mount would
// need root and would make unit tests depend on the machine running them.
type readOnlyPlatformProbe struct {
	// exists reports whether a path is present. Helpers, filesystem bundles.
	exists func(path string) bool
	// kernelFeature reports whether the kernel exposes a name (sysctl oid). It is
	// diagnostic only and never decides hasNullfs; see the comment there.
	kernelFeature func(name string) bool
}

var readOnlyNullfsHelpers = []string{
	"/sbin/mount_nullfs",
	"/usr/sbin/mount_nullfs",
	"/usr/local/sbin/mount_nullfs",
	"/sbin/mount_null",
	"/usr/sbin/mount_null",
}

var readOnlyNullfsBundles = []string{
	"/Library/Filesystems/null.fs",
	"/System/Library/Filesystems/null.fs",
}

var readOnlyNullfsKernelFeatures = []string{
	"vfs.generic.nullfs",
}

// kernelRegistersNullfs is what the sysctl oid is good for: a wording input, so a
// refusal can tell an operator whether the kernel claims the code is there while the
// mount route is not. It is not evidence that a read-only mount can be made.
func (p readOnlyPlatformProbe) kernelRegistersNullfs() bool {
	if p.kernelFeature == nil {
		return false
	}
	for _, name := range readOnlyNullfsKernelFeatures {
		if p.kernelFeature(name) {
			return true
		}
	}
	return false
}

// hasNullfs reports whether this OS has a route a mount(8) call or helper can actually
// take to a read-only null mount: a mount helper, or a filesystem bundle for mount(8)
// to exec. A kernel sysctl oid is deliberately not enough, and dropping that branch
// closes the one way maclet could outvote a kernel that refuses the mount. Two
// measurements on the icarus node say why: `sysctl vfs.generic` lists tunables rather
// than registered filesystems (it omits devfs, which mounts and unmounts cleanly), so
// the oid is not a mount registry; and this kernel does have nullfs and still answers
// EPERM at the mount entry point, because nullfs_mount() requires the private
// entitlement com.apple.private.nullfs_allow. Registration and enforceability are
// different questions, and only the second one licenses a `:ro` promise.
func (p readOnlyPlatformProbe) hasNullfs() bool {
	for _, path := range readOnlyNullfsHelpers {
		if p.exists(path) {
			return true
		}
	}
	for _, path := range readOnlyNullfsBundles {
		if p.exists(path) {
			return true
		}
	}
	return false
}

// defaultReadOnlyPlatformProbe checks the running system. maclet's own node is
// always Darwin; off Darwin there is nothing to measure, and unit tests should run
// wherever the developer runs them rather than depend on the machine's kernel. maclet
// is a Darwin node agent, so the same function serves production and, with the fields
// replaced, the unit tests.
func defaultReadOnlyPlatformProbe() readOnlyPlatformProbe {
	if runtime.GOOS != "darwin" {
		return readOnlyPlatformProbe{exists: func(string) bool { return true }, kernelFeature: func(string) bool { return true }}
	}
	return readOnlyPlatformProbe{
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		kernelFeature: func(name string) bool {
			return exec.Command("sysctl", "-n", name).Run() == nil
		},
	}
}

// manifestRefusal marks an error whose cause is the Pod manifest rather than the
// node: maclet's parity policy refuses the shape outright. Reconcile reports
// these as MacletUnsupportedPod, which clients see as CreateContainerConfigError
// -- the signal kubelet gives for a container a node cannot configure -- instead
// of RunContainerError, which means the runtime tried to launch it and failed.
type manifestRefusal struct {
	message string
}

func (e *manifestRefusal) Error() string { return e.message }

func refuseManifest(format string, args ...any) error {
	return &manifestRefusal{message: fmt.Sprintf(format, args...)}
}

// mackerVolumeArgs builds the `-v` arguments for one container. refusalDetail is the
// node-specific cause behind a negative read-only capability answer, carried into the
// refusal so an operator reads what was measured rather than a generic sentence.
func mackerVolumeArgs(pod Pod, container ContainerSpec, readOnlySupported bool, refusalDetail ...string) ([]string, error) {
	volumes := make(map[string]string, len(pod.Spec.Volumes))
	volumeTypes := make(map[string]string, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "" {
			continue
		}
		if _, exists := volumeTypes[volume.Name]; exists {
			return nil, refuseManifest("Pod volume %q is declared more than once", volume.Name)
		}
		if volume.HostPath == nil {
			volumeTypes[volume.Name] = ""
			continue
		}
		volumeType := ""
		if volume.HostPath.Type != nil {
			volumeType = string(*volume.HostPath.Type)
		}
		hostPath, err := ensureHostPath(volume.HostPath.Path, volumeType)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		volumes[volume.Name] = hostPath
		volumeTypes[volume.Name] = volumeType
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
			return nil, refuseManifest("volume mount at %q has no volume name", mount.MountPath)
		}
		if mount.MountPath == "" || !filepath.IsAbs(mount.MountPath) {
			return nil, refuseManifest("volume mount %q must use an absolute mountPath", mount.MountPath)
		}
		mountPath := filepath.Clean(mount.MountPath)
		if _, exists := seenMountPaths[mountPath]; exists {
			return nil, refuseManifest("volume mount path %q is declared more than once", mountPath)
		}
		seenMountPaths[mountPath] = struct{}{}
		if mount.ReadOnly {
			if !readOnlySupported {
				cause := strings.TrimSpace(strings.Join(refusalDetail, " "))
				if cause == "" {
					cause = fmt.Sprintf("Macker must accept %q and this kernel must support read-only bind mounts, and maclet verifies both", mackerReadOnlyVolumeMarker)
				}
				return nil, refuseManifest("volume mount %q requests readOnly, but this node cannot enforce a read-only hostPath mount: %s. A hostPath volume on a Darwin node is a live host mapping, so running it writable would promise read-only and deliver the opposite; mount the data over NFS with readOnly, or drop readOnly (see KUBERNETES-PARITY.md)", mountPath, cause)
			}
		}
		if mount.SubPathExpr != "" {
			return nil, refuseManifest("volume mount %q uses subPathExpr, which maclet does not support", mountPath)
		}
		hostPath, exists := volumes[mount.Name]
		if !exists {
			if _, declared := volumeTypes[mount.Name]; declared {
				return nil, refuseManifest("volume %q is not a supported hostPath volume", mount.Name)
			}
			return nil, refuseManifest("volume mount %q references unknown volume %q", mountPath, mount.Name)
		}
		resolvedPath, err := resolveVolumeSubPath(hostPath, mount.SubPath)
		if err != nil {
			return nil, fmt.Errorf("volume mount %q: %w", mountPath, err)
		}
		if strings.Contains(resolvedPath, ":") || strings.Contains(mountPath, ":") {
			return nil, refuseManifest("volume mount %q cannot be passed to Macker because its path contains ':'", mountPath)
		}

		spec := resolvedPath + ":" + mountPath
		if mount.ReadOnly {
			// macOS mount_nullfs makes a directory subtree read-only; it cannot
			// mount a single file that way, so Macker would fail mid-start. maclet
			// refuses the manifest at admission instead, including the case where a
			// subPath of a Directory resolves to a file.
			info, err := os.Stat(resolvedPath)
			if err != nil {
				return nil, fmt.Errorf("volume mount %q: %w", mountPath, err)
			}
			if !info.IsDir() {
				return nil, refuseManifest("volume mount %q requests readOnly on %q, which is not a directory: macOS mount_nullfs cannot mount a single file read-only, so mount the containing directory with subPath and point the mount at it", mountPath, resolvedPath)
			}
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	return args, nil
}

// hostPortIsHostNetworkDefault reports whether a declared hostPort is the value the
// API server writes by itself for a hostNetwork Pod -- hostPort defaults to
// containerPort there -- rather than a host-port mapping the manifest asked for.
// maclet does not implement host-port mapping, but rejecting the default would make
// any hostNetwork Pod with ports impossible on this node class, which upstream
// Kubernetes plainly allows.
func hostPortIsHostNetworkDefault(hostNetwork bool, port ContainerPort) bool {
	return hostNetwork && port.HostPort == port.ContainerPort
}

func (m *workloadManager) runArgs(pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	return m.runArgsWithContext(context.Background(), pod, container, managed)
}

func (m *workloadManager) runArgsWithContext(ctx context.Context, pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	if err := validateNativePodNetwork(pod); err != nil {
		return nil, err
	}
	if pod.Spec.HostNetwork {
		return m.hostNetworkRunArgs(ctx, pod, container, managed)
	}
	if container.Image == "" {
		return nil, errors.New("container image is empty")
	}
	workingDir := ""
	if container.WorkingDir != "" {
		if !filepath.IsAbs(container.WorkingDir) {
			return nil, refuseManifest("container %q workingDir %q must be an absolute path", container.Name, container.WorkingDir)
		}
		workingDir = filepath.Clean(container.WorkingDir)
	}
	volumeArgs, err := m.mackerVolumeArgsWithContext(ctx, pod, container, managed)
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
	if workingDir != "" {
		args = append(args, "--workdir", workingDir)
	}
	environment, err := m.resolveContainerEnvironment(ctx, pod, container, managed)
	if err != nil {
		return nil, err
	}
	for _, value := range environment {
		args = append(args, "--env", value)
	}
	// Native workloads use per-Pod PF remapping by default. Opt out only for
	// images that cannot consume the MACKER_PORT_N value (for example, an
	// application with a hard-coded listener port).
	portForward := pod.ObjectMeta.Annotations[nativeDisablePortForwardAnnotation] != "true"
	for index, port := range container.Ports {
		if port.HostPort != 0 && !hostPortIsHostNetworkDefault(pod.Spec.HostNetwork, port) {
			return nil, refuseManifest("container port %d requests hostPort %d; maclet does not support host-port mapping yet", port.ContainerPort, port.HostPort)
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return nil, refuseManifest("container port %d is outside the valid range", port.ContainerPort)
		}
		protocol := strings.ToUpper(string(port.Protocol))
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" && protocol != "UDP" {
			return nil, refuseManifest("container port %d uses unsupported protocol %q", port.ContainerPort, port.Protocol)
		}
		// Macker allocates a unique process port and PF-redirects PodIP:port
		// to it, allowing overlapping rollout generations. An explicitly
		// opted-out workload receives its declared port directly instead.
		if portForward {
			args = append(args, "-p", fmt.Sprintf("%d:auto/%s", port.ContainerPort, strings.ToLower(protocol)))
		} else {
			args = append(args, "--env", fmt.Sprintf("MACKER_PORT_%d=%d", index+1, port.ContainerPort))
		}
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

func (m *workloadManager) hostNetworkRunArgs(ctx context.Context, pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	if container.Image == "" {
		return nil, errors.New("container image is empty")
	}
	workingDir := ""
	if container.WorkingDir != "" {
		if !filepath.IsAbs(container.WorkingDir) {
			return nil, refuseManifest("container %q workingDir %q must be an absolute path", container.Name, container.WorkingDir)
		}
		workingDir = filepath.Clean(container.WorkingDir)
	}
	volumeArgs, err := m.mackerVolumeArgsWithContext(ctx, pod, container, managed)
	if err != nil {
		return nil, fmt.Errorf("container %q: %w", container.Name, err)
	}
	environment, err := m.resolveContainerEnvironment(ctx, pod, container, managed)
	if err != nil {
		return nil, err
	}
	args := []string{"run", "--detach", "--net=host", "--name", managed.ContainerName}
	if workingDir != "" {
		args = append(args, "--workdir", workingDir)
	}
	args = append(args, volumeArgs...)
	for _, value := range environment {
		args = append(args, "--env", value)
	}
	// With hostNetwork the declared container port *is* the host port, which is what
	// ordinary Kubernetes does. Macker's `-p CONTAINER:auto/tcp` shape would move
	// the process onto an allocated port and PF-redirect the declared port to it, so
	// nothing would ever listen on the port the Pod asked for. Publish no mapping and
	// hand each token its declared value directly; Macker installs PF rules only when
	// -p is present. nativeDisablePortForward has no meaning on this path, because it
	// never forwards.
	for index, port := range container.Ports {
		if port.HostPort != 0 && !hostPortIsHostNetworkDefault(pod.Spec.HostNetwork, port) {
			return nil, refuseManifest("container port %d requests hostPort %d; maclet does not support host-port mapping yet", port.ContainerPort, port.HostPort)
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return nil, refuseManifest("container port %d is outside the valid range", port.ContainerPort)
		}
		protocol := strings.ToUpper(string(port.Protocol))
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" && protocol != "UDP" {
			return nil, refuseManifest("container port %d uses unsupported protocol %q", port.ContainerPort, port.Protocol)
		}
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
