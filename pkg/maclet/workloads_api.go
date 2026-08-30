package maclet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type Workload struct {
	Namespace      string   `json:"namespace"`
	Name           string   `json:"name"`
	UID            string   `json:"uid,omitempty"`
	Phase          string   `json:"phase,omitempty"`
	PodIP          string   `json:"podIP,omitempty"`
	HostIP         string   `json:"hostIP,omitempty"`
	Containers     []string `json:"containers,omitempty"`
	InitContainers []string `json:"initContainers,omitempty"`
}

type WorkloadSnapshot struct {
	Node        string     `json:"node"`
	GeneratedAt time.Time  `json:"generatedAt"`
	Workloads   []Workload `json:"workloads"`
}

func workloadSnapshot(ctx context.Context, client *APIClient, nodeName string) (WorkloadSnapshot, error) {
	query := url.Values{"fieldSelector": []string{"spec.nodeName=" + nodeName}}
	body, err := client.Get(ctx, "/api/v1/pods?"+query.Encode())
	if err != nil {
		return WorkloadSnapshot{}, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return WorkloadSnapshot{}, fmt.Errorf("decode PodList: %w", err)
	}
	snapshot := WorkloadSnapshot{Node: nodeName, GeneratedAt: time.Now().UTC(), Workloads: make([]Workload, 0, len(pods.Items))}
	for _, pod := range pods.Items {
		workload := Workload{Namespace: pod.ObjectMeta.Namespace, Name: pod.ObjectMeta.Name, UID: string(pod.ObjectMeta.UID), Phase: string(pod.Status.Phase), PodIP: pod.Status.PodIP, HostIP: pod.Status.HostIP}
		for _, container := range pod.Spec.Containers {
			workload.Containers = append(workload.Containers, container.Name+"="+container.Image)
		}
		for _, container := range pod.Spec.InitContainers {
			workload.InitContainers = append(workload.InitContainers, container.Name+"="+container.Image)
		}
		snapshot.Workloads = append(snapshot.Workloads, workload)
	}
	return snapshot, nil
}

func runWorkloads(stateDir string, insecure bool) error {
	body, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return fmt.Errorf("read maclet state: %w (run maclet join first)", err)
	}
	var state LocalState
	if err := json.Unmarshal(body, &state); err != nil {
		return err
	}
	client, err := newAPIClient(state.Server, mustReadFile(state.CAFile), state.ClientCert, state.ClientKey, insecure, "", "")
	if err != nil {
		return err
	}
	snapshot, err := workloadSnapshot(context.Background(), client, state.NodeName)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}
