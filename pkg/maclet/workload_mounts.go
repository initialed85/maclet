package maclet

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	// Native workloads use per-Pod PF remapping by default. Opt out only for
	// images that cannot consume the MACKER_PORT_N value (for example, an
	// application with a hard-coded listener port).
	portForward := pod.ObjectMeta.Annotations[nativeDisablePortForwardAnnotation] != "true"
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
		protocol := strings.ToUpper(string(port.Protocol))
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" && protocol != "UDP" {
			return nil, fmt.Errorf("container port %d uses unsupported protocol %q", port.ContainerPort, port.Protocol)
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
