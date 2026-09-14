package maclet

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMackerConfigMapVolumeMaterializesItems(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/namespaces/demo/configmaps/settings" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "settings"},
			Data:       map[string]string{"app.conf": "port=8080\n", "ignored": "no"},
		})
	}))
	manager := newWorkloadManagerWithState(nil, "macker", "192.0.2.10", t.TempDir())
	manager.apiClient = client
	mode := int32(0o600)
	defaultMode := int32(0o640)
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native", UID: "uid-1"}, Spec: PodSpec{Volumes: []Volume{{Name: "settings", VolumeSource: VolumeSource{ConfigMap: &ConfigMapVolumeSource{
		LocalObjectReference: LocalObjectReference{Name: "settings"}, DefaultMode: &defaultMode,
		Items: []KeyToPath{{Key: "app.conf", Path: "config/app.conf", Mode: &mode}, {Key: "ignored", Path: "ignored"}},
	}}}}}}
	container := ContainerSpec{VolumeMounts: []VolumeMount{{Name: "settings", MountPath: "/etc/app", ReadOnly: true}}}
	managed := &managedWorkload{ContainerName: "native"}
	args, err := manager.mackerVolumeArgsWithContext(context.Background(), pod, container, managed)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "-v" || !strings.HasSuffix(args[1], ":/etc/app") {
		t.Fatalf("volume args = %#v", args)
	}
	volumePath := managed.VolumePaths[0]
	body, err := os.ReadFile(filepath.Join(volumePath, "config", "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "port=8080\n" {
		t.Fatalf("ConfigMap content = %q", body)
	}
	info, err := os.Stat(filepath.Join(volumePath, "config", "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ConfigMap mode = %o, want 600", info.Mode().Perm())
	}
	defaultInfo, err := os.Stat(filepath.Join(volumePath, "ignored"))
	if err != nil {
		t.Fatal(err)
	}
	if defaultInfo.Mode().Perm() != 0o640 {
		t.Fatalf("ConfigMap default mode = %o, want 640", defaultInfo.Mode().Perm())
	}
}

func TestMackerConfigMapVolumeRejectsMissingRequiredItem(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings"}, Data: map[string]string{"present": "yes"}})
	}))
	manager := newWorkloadManagerWithState(nil, "macker", "192.0.2.10", t.TempDir())
	manager.apiClient = client
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native", UID: "uid-1"}, Spec: PodSpec{Volumes: []Volume{{Name: "settings", VolumeSource: VolumeSource{ConfigMap: &ConfigMapVolumeSource{
		LocalObjectReference: LocalObjectReference{Name: "settings"},
		Items:                []KeyToPath{{Key: "missing", Path: "missing"}},
	}}}}}}
	_, err := manager.mackerVolumeArgsWithContext(context.Background(), pod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "settings", MountPath: "/etc/app"}}}, &managedWorkload{ContainerName: "native"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing ConfigMap item error = %v", err)
	}
}

func TestMackerConfigMapVolumeOptionalMissingIsEmpty(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { http.NotFound(response, request) }))
	manager := newWorkloadManagerWithState(nil, "macker", "192.0.2.10", t.TempDir())
	manager.apiClient = client
	optional := true
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native", UID: "uid-1"}, Spec: PodSpec{Volumes: []Volume{{Name: "settings", VolumeSource: VolumeSource{ConfigMap: &ConfigMapVolumeSource{
		LocalObjectReference: LocalObjectReference{Name: "missing"}, Optional: &optional,
	}}}}}}
	managed := &managedWorkload{ContainerName: "native"}
	if _, err := manager.mackerVolumeArgsWithContext(context.Background(), pod, ContainerSpec{VolumeMounts: []VolumeMount{{Name: "settings", MountPath: "/etc/app"}}}, managed); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(managed.VolumePaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("optional missing ConfigMap materialization = %#v, want empty", entries)
	}
}
