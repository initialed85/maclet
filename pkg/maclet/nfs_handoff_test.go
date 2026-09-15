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
		nfsHandoffGenerationAnnotation: "7",
	}
}

func TestResolveNFSVolumeForPodUsesGenericHandoff(t *testing.T) {
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Annotations: completeNFSPodAnnotations()}, Spec: PodSpec{Volumes: []Volume{{Name: "data", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "data", ReadOnly: true}}}}}}
	got, err := resolveNFSVolumeForPod(context.Background(), nil, pod, pod.Spec.Volumes[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "10.43.1.70" || got.Share != "/export" || !got.ReadOnly || !strings.Contains(strings.Join(got.MountOptions, ","), "mountport=20048") {
		t.Fatalf("handoff = %#v", got)
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
