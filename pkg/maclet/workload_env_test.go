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

func TestResolveContainerEnvironmentFromReferencesAndDownwardAPI(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/namespaces/demo/configmaps/settings":
			_ = json.NewEncoder(response).Encode(ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings"}, Data: map[string]string{"Z_LAST": "z", "A_FIRST": "a"}})
		case "/api/v1/namespaces/demo/secrets/credentials":
			_ = json.NewEncoder(response).Encode(Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials"}, Data: map[string][]byte{"TOKEN": []byte("secret-value")}})
		default:
			http.NotFound(response, request)
		}
	}))
	manager := newWorkloadManager(nil, "macker", "192.0.2.10")
	manager.apiClient = client
	pod := Pod{
		ObjectMeta: ObjectMeta{
			Namespace:   "demo",
			Name:        "native",
			UID:         "pod-uid",
			Labels:      map[string]string{"app": "web"},
			Annotations: map[string]string{"example.test/value": "annotation"},
		},
		Spec:   PodSpec{NodeName: "maclet", ServiceAccountName: "default"},
		Status: PodStatus{PodIP: "10.42.8.3"},
	}
	optional := true
	container := ContainerSpec{
		EnvFrom: []EnvFromSource{
			{Prefix: "CFG_", ConfigMapRef: &ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}},
			{Prefix: "SECRET_", SecretRef: &SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "credentials"}}},
			{ConfigMapRef: &ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Optional: &optional}},
		},
		Env: []EnvVar{
			{Name: "CFG_A_FIRST", Value: "override"},
			{Name: "POD_NAME", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "POD_UID", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "metadata.uid"}}},
			{Name: "NODE_NAME", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
			{Name: "SERVICE_ACCOUNT", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}},
			{Name: "POD_IP", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "HOST_IP", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "status.hostIP"}}},
			{Name: "APP", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}},
			{Name: "ANNOTATION", ValueFrom: &EnvVarSource{FieldRef: &ObjectFieldSelector{FieldPath: "metadata.annotations['example.test/value']"}}},
		},
	}
	values, err := manager.resolveContainerEnvironment(context.Background(), pod, container, &managedWorkload{IP: "10.42.8.3"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, "\x00")
	for _, want := range []string{
		"CFG_A_FIRST=override", "CFG_Z_LAST=z", "SECRET_TOKEN=secret-value", "POD_NAME=native",
		"POD_NAMESPACE=demo", "POD_UID=pod-uid", "NODE_NAME=maclet", "SERVICE_ACCOUNT=default",
		"POD_IP=10.42.8.3", "HOST_IP=192.0.2.10", "APP=web", "ANNOTATION=annotation",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("resolved environment lacks %q: %#v", want, values)
		}
	}
}

func TestResolveContainerEnvironmentRejectsUnsupportedValueFrom(t *testing.T) {
	manager := newWorkloadManager(nil, "macker", "192.0.2.10")
	container := ContainerSpec{Env: []EnvVar{{Name: "UNSUPPORTED", ValueFrom: &EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{}}}}}
	_, err := manager.resolveContainerEnvironment(context.Background(), Pod{}, container, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported valueFrom") {
		t.Fatalf("resolveContainerEnvironment() error = %v, want unsupported valueFrom", err)
	}
}

func TestResolveContainerEnvironmentFromKeySelectors(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/namespaces/demo/secrets/github":
			_ = json.NewEncoder(response).Encode(Secret{ObjectMeta: metav1.ObjectMeta{Name: "github"}, Data: map[string][]byte{"GH_TOKEN": []byte("gh-token-value")}})
		case "/api/v1/namespaces/demo/configmaps/settings":
			_ = json.NewEncoder(response).Encode(ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings"}, Data: map[string]string{"REGION": "eu-west-1"}})
		default:
			http.NotFound(response, request)
		}
	}))
	manager := newWorkloadManager(nil, "macker", "192.0.2.10")
	manager.apiClient = client
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native", UID: "pod-uid"}}
	optional := true
	container := ContainerSpec{Env: []EnvVar{
		{Name: "GH_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "github"}, Key: "GH_TOKEN"}}},
		{Name: "REGION", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "REGION"}}},
		{Name: "OPTIONAL_MISSING_SECRET", ValueFrom: &EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Key: "ANY", Optional: &optional}}},
		{Name: "OPTIONAL_MISSING_CONFIG_MAP", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Key: "ANY", Optional: &optional}}},
	}}
	values, err := manager.resolveContainerEnvironment(context.Background(), pod, container, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, "\x00")
	for _, want := range []string{"GH_TOKEN=gh-token-value", "REGION=eu-west-1", "OPTIONAL_MISSING_SECRET=", "OPTIONAL_MISSING_CONFIG_MAP="} {
		if !strings.Contains(joined, want) {
			t.Errorf("resolved environment lacks %q: %#v", want, values)
		}
	}
}

func TestResolveContainerEnvironmentKeySelectorFailures(t *testing.T) {
	client := ownershipTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/namespaces/demo/secrets/present" {
			_ = json.NewEncoder(response).Encode(Secret{ObjectMeta: metav1.ObjectMeta{Name: "present"}, Data: map[string][]byte{"THERE": []byte("ok")}})
			return
		}
		if request.URL.Path == "/api/v1/namespaces/demo/configmaps/present" {
			_ = json.NewEncoder(response).Encode(ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "present"}, Data: map[string]string{"THERE": "ok"}})
			return
		}
		http.NotFound(response, request)
	}))
	manager := newWorkloadManager(nil, "macker", "192.0.2.10")
	manager.apiClient = client
	pod := Pod{ObjectMeta: ObjectMeta{Namespace: "demo", Name: "native"}}
	cases := []struct {
		name    string
		env     EnvVar
		wantErr string
	}{
		{"missing Secret is fatal unless optional", EnvVar{Name: "GH_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Key: "GH_TOKEN"}}}, "Secret \"gone\""},
		{"missing ConfigMap is fatal unless optional", EnvVar{Name: "REGION", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gone"}, Key: "REGION"}}}, "ConfigMap \"gone\""},
		{"missing key in Secret is fatal", EnvVar{Name: "GH_TOKEN", ValueFrom: &EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "present"}, Key: "ABSENT"}}}, "has no key \"ABSENT\""},
		{"missing key in ConfigMap is fatal", EnvVar{Name: "REGION", ValueFrom: &EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "present"}, Key: "ABSENT"}}}, "ConfigMap \"present\" has no key \"ABSENT\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manager.resolveContainerEnvironment(context.Background(), pod, ContainerSpec{Env: []EnvVar{tc.env}}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("resolveContainerEnvironment() error = %v, want it to mention %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "gh-token-value") {
				t.Errorf("error text leaks a Secret value: %v", err)
			}
		})
	}
}

func TestRedactMackerArgsRedactsEnvironmentValues(t *testing.T) {
	got := redactMackerArgs([]string{"run", "--env", "TOKEN=secret-value", "--env", "EMPTY", "image"})
	if strings.Contains(got, "secret-value") || !strings.Contains(got, "TOKEN=<redacted>") {
		t.Fatalf("redacted invocation = %q", got)
	}
}
