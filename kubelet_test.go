package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestKubeletPathParts(t *testing.T) {
	namespace, pod, container, ok := kubeletPathParts("/containerLogs/default/nginx%2Dpod/nginx", kubeletContainerLogsPrefix)
	if !ok || namespace != "default" || pod != "nginx-pod" || container != "nginx" {
		t.Fatalf("parts = %q/%q/%q, ok=%v", namespace, pod, container, ok)
	}
	if _, _, _, ok := kubeletPathParts("/containerLogs/default/only-two", kubeletContainerLogsPrefix); ok {
		t.Fatal("accepted incomplete kubelet path")
	}
}

func TestKubeletQueryBool(t *testing.T) {
	query := url.Values{"input": {"1"}, "output": {"true"}}
	if !kubeletQueryBool(query, "stdin", "input") || !kubeletQueryBool(query, "stdout", "output") {
		t.Fatalf("query = %v", query)
	}
	if kubeletQueryBool(url.Values{"tty": {"false"}}, "tty") {
		t.Fatal("false query value was accepted")
	}
}

func TestKubeletExecStatusIncludesWrappedExitCode(t *testing.T) {
	var output bytes.Buffer
	writeKubeletExecStatus(&output, errors.New("workload exited: exit status 7"))
	var status struct {
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Reason != "NonZeroExitCode" || len(status.Details.Causes) != 1 || status.Details.Causes[0].Reason != "ExitCode" || status.Details.Causes[0].Message != "7" {
		t.Fatalf("status = %s", output.String())
	}
}

func TestKubeletLogsDelegatesToMacker(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "macker")
	script := `#!/bin/sh
case "$1" in
ps)
  printf 'macker-default-nginx\timage\trunning\n'
  ;;
logs)
  printf 'hello from nginx\n'
  ;;
*)
  exit 1
  ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := newWorkloadManager(nil, binary, "192.168.137.111")
	manager.workloads["uid-1"] = &managedWorkload{
		UID: "uid-1", Namespace: "default", Name: "nginx-pod",
		PodContainerName: "nginx", ContainerName: "macker-default-nginx",
	}
	handler := &kubeletHandler{manager: manager}
	request := httptest.NewRequest(http.MethodGet, "/containerLogs/default/nginx-pod/nginx", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "hello from nginx\n" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestKubeletLogsRejectsUnmanagedContainer(t *testing.T) {
	manager := newWorkloadManager(nil, "/does/not/exist", "192.168.137.111")
	manager.workloads["uid-1"] = &managedWorkload{
		UID: "uid-1", Namespace: "default", Name: "nginx-pod",
		PodContainerName: "nginx", ContainerName: "macker-default-nginx",
	}
	handler := &kubeletHandler{manager: manager}
	request := httptest.NewRequest(http.MethodGet, "/containerLogs/default/nginx-pod/other", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
}
