package maclet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (m *workloadManager) mountGenericNFSVolume(ctx context.Context, pod Pod, volumeName string, source *PersistentVolumeClaimVolumeSource, managed *managedWorkload) (string, error) {
	if source == nil || source.ClaimName == "" {
		return "", fmt.Errorf("volume %q has no PVC claimName", volumeName)
	}
	if m.journalPath == "" || m.nfsRoot == "" {
		return "", errors.New("NFS volumes require a state-backed workload manager")
	}
	resolved, err := resolveNFSVolumeForPod(ctx, m.apiClient, pod, Volume{VolumeSource: VolumeSource{PersistentVolumeClaim: source}})
	if err != nil {
		return "", err
	}
	volumeKey := sanitizeVolumeKey(volumeName)
	if volumeKey == "" {
		return "", fmt.Errorf("volume name %q is invalid", volumeName)
	}
	target := filepath.Join(m.nfsRoot, workloadContainerName(pod), volumeKey)
	spec := nfsMountSpec{Server: resolved.Server, Share: resolved.Share, MountOptions: resolved.MountOptions, ReadOnly: resolved.ReadOnly}
	probe := m.isMountpoint
	if probe == nil {
		probe = probeNFSMount
	}
	if mounted, probeErr := probe(target, spec); probeErr == nil && mounted {
		if managed != nil {
			managed.NFSMounts = appendMountRecord(managed.NFSMounts, nfsMountRecord{Target: target, Spec: spec})
		}
		return filepath.Join(target, filepath.FromSlash(resolved.Subdir)), nil
	}
	if info, statErr := os.Stat(target); statErr == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("NFS mount target %s is not a directory", target)
		}
		entries, readErr := os.ReadDir(target)
		if readErr != nil {
			return "", fmt.Errorf("inspect NFS mount target %s: %w", target, readErr)
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("NFS mount target %s contains an unowned directory", target)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect NFS mount target %s: %w", target, statErr)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return "", fmt.Errorf("create NFS mount target %s: %w", target, err)
	}
	mount := m.mountNFS
	if mount == nil {
		mount = func(ctx context.Context, useSudo bool, spec nfsMountSpec, target string) error {
			return runNFSMount(ctx, useSudo, spec, target)
		}
	}
	if err := mount(ctx, m.useSudo, spec, target); err != nil {
		_ = os.Remove(target)
		return "", err
	}
	if managed != nil {
		managed.NFSMounts = appendMountRecord(managed.NFSMounts, nfsMountRecord{Target: target, Spec: spec})
	}
	return filepath.Join(target, filepath.FromSlash(resolved.Subdir)), nil
}

func appendMountRecord(records []nfsMountRecord, record nfsMountRecord) []nfsMountRecord {
	for _, existing := range records {
		if existing.Target == record.Target && existing.Spec.Server == record.Spec.Server && existing.Spec.Share == record.Spec.Share && existing.Spec.ReadOnly == record.Spec.ReadOnly && strings.Join(existing.Spec.MountOptions, "\x00") == strings.Join(record.Spec.MountOptions, "\x00") {
			return records
		}
	}
	return append(records, record)
}

func probeNFSMount(target string, spec nfsMountSpec) (bool, error) {
	output, err := exec.Command("mount").Output()
	if err != nil {
		return false, err
	}
	needle := " on " + target + " "
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, needle) && strings.Contains(line, spec.Server+":"+spec.Share) {
			return true, nil
		}
	}
	return false, nil
}
