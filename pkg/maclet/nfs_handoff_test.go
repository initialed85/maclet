package maclet

import (
	"context"
	"strings"
	"testing"
)

func completeNFSPodAnnotations() map[string]string {
	return map[string]string{
		nfsHandoffServerAnnotation:     "10.43.1.70",
		nfsHandoffExportAnnotation:     "/export",
		nfsHandoffVersionAnnotation:    "3",
		nfsHandoffMountPortAnnotation:  "20048",
		nfsHandoffNFSPortAnnotation:    "2049",
		nfsHandoffGenerationAnnotation: "7",
	}
}

func TestResolveNFSVolumeForPodUsesGenericHandoff(t *testing.T) {
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Annotations: completeNFSPodAnnotations()}, Spec: PodSpec{Volumes: []Volume{{Name: "data", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "data", ReadOnly: true}}}}}}
	got, err := resolveNFSVolumeForPod(context.Background(), nil, pod, pod.Spec.Volumes[0])
	if err != nil {
		t.Fatal(err)
	}
	options := strings.Join(got.MountOptions, ",")
	if !got.LegacyFlags {
		t.Fatal("handoff did not request legacy macOS mount flags")
	}
	if got.Server != "10.43.1.70" || got.Share != "/export" || !got.ReadOnly || !strings.Contains(options, "mountport=20048") || !strings.Contains(options, "locallocks") || !strings.Contains(options, "resvport") {
		t.Fatalf("handoff = %#v", got)
	}
	if args, err := nfsMountCommandArgs(nfsMountSpec{Server: got.Server, Share: got.Share, MountOptions: got.MountOptions, ReadOnly: got.ReadOnly, LegacyFlags: got.LegacyFlags}, "/tmp/gateway-nfs"); err != nil {
		t.Fatalf("handoff mount command validation = %v", err)
	} else if strings.Join(args, "\x00") != "mount_nfs\x00-L\x00-P\x00-T\x00-3\x00-o\x00mountport=20048,port=2049,ro\x0010.43.1.70:/export\x00/tmp/gateway-nfs" {
		t.Fatalf("handoff mount command = %#v", args)
	}
}

func TestResolveNFSVolumeForPodRejectsIncompleteOrStaleHandoff(t *testing.T) {
	annotations := completeNFSPodAnnotations()
	delete(annotations, nfsHandoffGenerationAnnotation)
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Annotations: annotations}, Spec: PodSpec{Volumes: []Volume{{Name: "data", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}
	_, err := resolveNFSVolumeForPod(context.Background(), nil, pod, pod.Spec.Volumes[0])
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete handoff error = %v", err)
	}
	annotations = completeNFSPodAnnotations()
	annotations[nfsHandoffVersionAnnotation] = "4"
	pod.ObjectMeta.Annotations = annotations
	_, err = resolveNFSVolumeForPod(context.Background(), nil, pod, pod.Spec.Volumes[0])
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("stale/invalid handoff error = %v", err)
	}
}

func TestResolveNFSVolumeForPodRejectsAmbiguousMultiPVCHandoff(t *testing.T) {
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Annotations: completeNFSPodAnnotations()}, Spec: PodSpec{Volumes: []Volume{
		{Name: "one", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "one"}}},
		{Name: "two", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "two"}}},
	}}}
	_, err := resolveNFSVolumeForPod(context.Background(), nil, pod, pod.Spec.Volumes[0])
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multi-PVC handoff error = %v", err)
	}
}
