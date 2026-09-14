package maclet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (m *workloadManager) mackerVolumeArgsWithContext(ctx context.Context, pod Pod, container ContainerSpec, managed *managedWorkload) ([]string, error) {
	podCopy := pod
	podCopy.Spec.Volumes = append([]Volume(nil), pod.Spec.Volumes...)
	containerCopy := container
	containerCopy.VolumeMounts = append([]VolumeMount(nil), container.VolumeMounts...)
	materialized := make([]string, 0)
	configMapVolumes := make(map[string]bool)
	for index := range podCopy.Spec.Volumes {
		volume := &podCopy.Spec.Volumes[index]
		if volume.ConfigMap == nil {
			continue
		}
		path, err := m.materializeConfigMapVolume(ctx, pod, volume.Name, volume.ConfigMap)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		materialized = append(materialized, path)
		configMapVolumes[volume.Name] = true
		volume.HostPath = &HostPathVolumeSource{Path: path}
		volume.ConfigMap = nil
	}
	for index := range containerCopy.VolumeMounts {
		mount := &containerCopy.VolumeMounts[index]
		if configMapVolumes[mount.Name] {
			// ConfigMap data is materialized into a private trusted-native
			// directory. Macker cannot enforce read-only symlink mounts, but
			// writes remain local to this materialization and never update the
			// Kubernetes ConfigMap.
			mount.ReadOnly = false
		}
	}
	args, err := mackerVolumeArgs(podCopy, containerCopy)
	if err != nil {
		return nil, err
	}
	if managed != nil {
		m.replaceManagedVolumePaths(managed, materialized)
	}
	return args, nil
}

// refreshConfigMapVolumes updates already-running trusted workloads in place.
// Macker's rootfs symlink points at the deterministic target directory, so an
// atomic target replacement makes new ConfigMap content visible without
// restarting the native process.
func (m *workloadManager) refreshConfigMapVolumes(ctx context.Context, pod Pod, managed *managedWorkload) error {
	paths := make([]string, 0)
	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap == nil {
			continue
		}
		path, err := m.materializeConfigMapVolume(ctx, pod, volume.Name, volume.ConfigMap)
		if err != nil {
			return fmt.Errorf("volume %q: %w", volume.Name, err)
		}
		paths = append(paths, path)
	}
	m.replaceManagedVolumePaths(managed, paths)
	return nil
}

func (m *workloadManager) replaceManagedVolumePaths(managed *managedWorkload, paths []string) {
	if managed == nil {
		return
	}
	keep := make(map[string]bool, len(paths))
	for _, path := range paths {
		keep[path] = true
	}
	for _, path := range managed.VolumePaths {
		if !keep[path] {
			_ = os.RemoveAll(path)
		}
	}
	managed.VolumePaths = append([]string(nil), paths...)
}

func (m *workloadManager) materializeConfigMapVolume(ctx context.Context, pod Pod, volumeName string, source *ConfigMapVolumeSource) (string, error) {
	if source == nil {
		return "", fmt.Errorf("ConfigMap source is empty")
	}
	if m.journalPath == "" || m.volumeRoot == "" {
		return "", fmt.Errorf("ConfigMap volumes require a state-backed workload manager")
	}
	configMap, found, err := m.getConfigMap(ctx, pod.ObjectMeta.Namespace, source.Name, source.Optional != nil && *source.Optional)
	if err != nil {
		return "", err
	}
	if !found {
		configMap = ConfigMap{}
	}
	podKey := workloadContainerName(pod)
	volumeKey := sanitizeVolumeKey(volumeName)
	if volumeKey == "" {
		return "", fmt.Errorf("volume name %q is invalid", volumeName)
	}
	base := filepath.Join(m.volumeRoot, podKey)
	target := filepath.Join(base, volumeKey)
	staging := filepath.Join(base, fmt.Sprintf(".%s.tmp-%d", volumeKey, time.Now().UnixNano()))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create materialized volume directory: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", fmt.Errorf("create materialized ConfigMap staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(staging)
		}
	}()

	data := make(map[string][]byte, len(configMap.Data)+len(configMap.BinaryData))
	for key, value := range configMap.Data {
		data[key] = []byte(value)
	}
	for key, value := range configMap.BinaryData {
		data[key] = append([]byte(nil), value...)
	}
	items := source.Items
	if len(items) == 0 {
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := writeConfigMapFile(staging, key, data[key], source.DefaultMode); err != nil {
				return "", err
			}
		}
	} else {
		for _, item := range items {
			value, ok := data[item.Key]
			if !ok {
				return "", fmt.Errorf("ConfigMap key %q is missing", item.Key)
			}
			mode := item.Mode
			if mode == nil {
				mode = source.DefaultMode
			}
			if err := writeConfigMapFile(staging, item.Path, value, mode); err != nil {
				return "", err
			}
		}
	}
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("replace materialized ConfigMap volume: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		return "", fmt.Errorf("install materialized ConfigMap volume: %w", err)
	}
	cleanup = false
	return target, nil
}

func sanitizeVolumeKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return ""
	}
	return value
}

func writeConfigMapFile(root, relative string, content []byte, mode *int32) error {
	if relative == "" || filepath.IsAbs(relative) {
		return fmt.Errorf("ConfigMap item path %q must be relative", relative)
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("ConfigMap item path %q escapes the volume", relative)
	}
	if strings.Contains(clean, ":") {
		return fmt.Errorf("ConfigMap item path %q contains ':'", relative)
	}
	path := filepath.Join(root, clean)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ConfigMap item parent %q: %w", relative, err)
	}
	fileMode := int32(0o644)
	if mode != nil {
		fileMode = *mode
	}
	if fileMode < 0 || fileMode&^0o777 != 0 {
		return fmt.Errorf("ConfigMap item %q has invalid mode %o", relative, fileMode)
	}
	if err := os.WriteFile(path, content, os.FileMode(fileMode)); err != nil {
		return fmt.Errorf("write ConfigMap item %q: %w", relative, err)
	}
	return nil
}
