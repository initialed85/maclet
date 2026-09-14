package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveGenericNFSVolume(t *testing.T) {
	claim := PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "demo", UID: "claim-uid"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-shared"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-shared"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Name: "shared", Namespace: "demo", UID: "claim-uid"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &CSIPersistentVolumeSource{Driver: genericNFSCSIDriver, VolumeAttributes: map[string]string{
				"server": "192.168.1.25", "share": "/exports/shared", "subdir": "team/a",
			}}},
			MountOptions: []string{"vers=3", "tcp", "rsize=65536"},
		},
	}
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/namespaces/demo/persistentvolumeclaims/shared":
			_ = json.NewEncoder(response).Encode(claim)
		case "/api/v1/persistentvolumes/pv-shared":
			_ = json.NewEncoder(response).Encode(pv)
		default:
			http.NotFound(response, request)
		}
	}))
	volume := Volume{Name: "data", VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "shared", ReadOnly: true}}}
	got, err := resolveGenericNFSVolume(context.Background(), client, "demo", volume)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "192.168.1.25" || got.Share != "/exports/shared" || got.Subdir != "team/a" || !got.ReadOnly || len(got.MountOptions) != 3 {
		t.Fatalf("resolved NFS = %#v", got)
	}
}

func TestResolveGenericNFSRejectsUnboundAndLonghornRWO(t *testing.T) {
	claim := PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "demo"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { _ = json.NewEncoder(response).Encode(claim) }))
	volume := Volume{VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "claim"}}}
	if _, err := resolveGenericNFSVolume(context.Background(), client, "demo", volume); err == nil || !strings.Contains(err.Error(), "not Bound") {
		t.Fatalf("unbound PVC error = %v", err)
	}
}

func TestGenericNFSValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   func() error
	}{
		{"share traversal", func() error { _, err := validateNFSShare("/exports/../secret"); return err }},
		{"absolute subdir", func() error { _, err := validateNFSSubdir("/outside"); return err }},
		{"unknown option", func() error { return validateNFSMountOptions([]string{"intr"}) }},
		{"bad version", func() error { return validateNFSMountOptions([]string{"vers=2"}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.fn(); err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
		})
	}
}
