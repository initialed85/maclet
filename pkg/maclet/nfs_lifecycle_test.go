package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenericNFSPVCWiringMountsAndPassesSubdirToMacker(t *testing.T) {
	claim := PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "demo", UID: "claim"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pv := PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv"}, Spec: corev1.PersistentVolumeSpec{
		ClaimRef:               &corev1.ObjectReference{Name: "shared", Namespace: "demo", UID: "claim"},
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &CSIPersistentVolumeSource{Driver: genericNFSCSIDriver, VolumeAttributes: map[string]string{"server": "192.168.1.25", "share": "/exports/share", "subdir": "team"}}},
	}}
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/namespaces/demo/persistentvolumeclaims/shared":
			_ = json.NewEncoder(response).Encode(claim)
		case "/api/v1/persistentvolumes/pv":
			_ = json.NewEncoder(response).Encode(pv)
		default:
			http.NotFound(response, request)
		}
	}))
	manager := newWorkloadManagerWithState(nil, "macker", "192.0.2.10", t.TempDir())
	manager.apiClient = client
	var mounted nfsMountSpec
	manager.mountNFS = func(_ context.Context, _ bool, spec nfsMountSpec, target string) error {
		mounted = spec
		return os.MkdirAll(filepath.Join(target, "team", "config"), 0o700)
	}
	manager.isMountpoint = func(string, nfsMountSpec) (bool, error) { return false, nil }
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native", UID: "uid"}, Spec: PodSpec{Volumes: []Volume{{Name: "data", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "shared", ReadOnly: true}}}}}}
	container := ContainerSpec{VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/data", ReadOnly: true, SubPath: "config"}}}
	managed := &managedWorkload{ContainerName: "native"}
	args, err := manager.mackerVolumeArgsWithContext(context.Background(), pod, container, managed)
	if err != nil {
		t.Fatal(err)
	}
	if mounted.Server != "192.168.1.25" || mounted.Share != "/exports/share" || !mounted.ReadOnly {
		t.Fatalf("mount spec = %#v", mounted)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, ":/data") || !strings.Contains(joined, "/team/config") {
		t.Fatalf("Macker NFS args = %#v", args)
	}
	if len(managed.NFSMounts) != 1 {
		t.Fatalf("NFS mount records = %#v", managed.NFSMounts)
	}
}
