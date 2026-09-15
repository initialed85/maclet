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

func TestConfigMapForbiddenRequiresExplicitPeerGuidance(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "forbidden", http.StatusForbidden)
	}))
	manager := newWorkloadManager(nil, "macker", "192.0.2.10")
	manager.apiClient = client
	_, _, err := manager.getConfigMap(context.Background(), "demo", "settings", false)
	if err == nil || !strings.Contains(err.Error(), "--peer-kubeconfig") {
		t.Fatalf("ConfigMap 403 error = %v", err)
	}
}

func TestGenericNFSForbiddenRequiresExplicitPeerGuidance(t *testing.T) {
	// The PVC succeeds, but the bound PV is denied, matching the common
	// restricted-controller failure seen on a live node.
	client403 := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/namespaces/demo/persistentvolumeclaims/data" {
			_ = json.NewEncoder(response).Encode(PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "demo"},
				Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			})
			return
		}
		http.Error(response, "forbidden", http.StatusForbidden)
	}))
	volume := Volume{VolumeSource: VolumeSource{PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}
	_, err := resolveGenericNFSVolume(context.Background(), client403, "demo", volume)
	if err == nil || !strings.Contains(err.Error(), "--peer-kubeconfig") {
		t.Fatalf("PV 403 error = %v", err)
	}
}
