package maclet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	nfsHandoffServerAnnotation     = "storage.k8s-darwin.dev/nfs-server"
	nfsHandoffExportAnnotation     = "storage.k8s-darwin.dev/nfs-export"
	nfsHandoffVersionAnnotation    = "storage.k8s-darwin.dev/nfs-version"
	nfsHandoffMountPortAnnotation  = "storage.k8s-darwin.dev/nfs-mount-port"
	nfsHandoffNFSPortAnnotation    = "storage.k8s-darwin.dev/nfs-port"
	nfsHandoffGenerationAnnotation = "storage.k8s-darwin.dev/nfs-generation"
)

func resolveNFSVolumeForPod(ctx context.Context, client *APIClient, pod Pod, volume Volume) (genericNFSVolume, error) {
	annotations := pod.ObjectMeta.Annotations
	keys := []string{nfsHandoffServerAnnotation, nfsHandoffExportAnnotation, nfsHandoffVersionAnnotation, nfsHandoffMountPortAnnotation, nfsHandoffNFSPortAnnotation, nfsHandoffGenerationAnnotation}
	present := 0
	for _, key := range keys {
		if strings.TrimSpace(annotations[key]) != "" {
			present++
		}
	}
	if present == 0 {
		return resolveGenericNFSVolume(ctx, client, pod.ObjectMeta.Namespace, volume)
	}
	if present != len(keys) {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff is incomplete; wait for the gateway endpoint or configure --peer-kubeconfig for direct CSI NFS")
	}
	if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName == "" {
		return genericNFSVolume{}, errors.New("generic NFS handoff requires a PVC-backed volume")
	}
	pvcVolumes := 0
	for _, candidate := range pod.Spec.Volumes {
		if candidate.PersistentVolumeClaim != nil {
			pvcVolumes++
		}
	}
	if pvcVolumes != 1 {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff is ambiguous for Pod with %d PVC-backed volumes", pvcVolumes)
	}
	version, err := strconv.Atoi(strings.TrimSpace(annotations[nfsHandoffVersionAnnotation]))
	if err != nil || version != 3 {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff has unsupported version %q; want 3", annotations[nfsHandoffVersionAnnotation])
	}
	mountPort, err := strconv.Atoi(strings.TrimSpace(annotations[nfsHandoffMountPortAnnotation]))
	if err != nil || mountPort < 1024 || mountPort > 65535 {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff has invalid mount port %q", annotations[nfsHandoffMountPortAnnotation])
	}
	nfsPort, err := strconv.Atoi(strings.TrimSpace(annotations[nfsHandoffNFSPortAnnotation]))
	if err != nil || nfsPort < 1024 || nfsPort > 65535 {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff has invalid NFS port %q", annotations[nfsHandoffNFSPortAnnotation])
	}
	server, err := validateNFSServer(annotations[nfsHandoffServerAnnotation])
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff server: %w", err)
	}
	share, err := validateNFSShare(annotations[nfsHandoffExportAnnotation])
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("generic NFS handoff export: %w", err)
	}
	return genericNFSVolume{
		ClaimName:    volume.PersistentVolumeClaim.ClaimName,
		Server:       server,
		Share:        share,
		MountOptions: []string{"mountport=" + strconv.Itoa(mountPort), "port=" + strconv.Itoa(nfsPort), "vers=3", "tcp", "locallocks", "resvport"},
		ReadOnly:     volume.PersistentVolumeClaim.ReadOnly,
		LegacyFlags:  true,
	}, nil
}
