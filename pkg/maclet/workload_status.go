package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podStatusPath(pod Pod) string {
	return "/api/v1/namespaces/" + url.PathEscape(pod.ObjectMeta.Namespace) + "/pods/" + url.PathEscape(pod.ObjectMeta.Name) + "/status"
}

func podPath(pod Pod) string {
	return "/api/v1/namespaces/" + url.PathEscape(pod.ObjectMeta.Namespace) + "/pods/" + url.PathEscape(pod.ObjectMeta.Name)
}

func setPodCondition(conditions []PodCondition, condition PodCondition) []PodCondition {
	updated := make([]PodCondition, 0, len(conditions)+1)
	found := false
	for _, existing := range conditions {
		if existing.Type == condition.Type {
			if !found {
				updated = append(updated, condition)
				found = true
			}
			continue
		}
		updated = append(updated, existing)
	}
	if !found {
		updated = append(updated, condition)
	}
	return updated
}

func desiredPodStatus(pod Pod, nodeIP, phase, ip, reason, message string, running bool, restartCount int32, termination ...*mackerInspection) PodStatus {
	status := pod.Status
	var inspection *mackerInspection
	if len(termination) > 0 {
		inspection = termination[0]
	}
	status.Phase = corev1.PodPhase(phase)
	status.PodIP = ip
	status.PodIPs = nil
	if ip != "" {
		status.PodIPs = []PodIP{{IP: ip}}
	}
	status.HostIP = nodeIP
	// PodStatus.reason is optional and has no Maclet-specific value. Leaving it
	// empty lets clients and kubectl use the canonical Pod phase (Running,
	// Pending, Succeeded, or Failed) for the top-level status. The native
	// workload identity is already carried by the Pod label and annotations.
	status.Reason = ""
	status.Message = message
	stamp := metav1.NewTime(time.Now().UTC())
	ready := running && phase == "Running"
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "PodScheduled", Status: "True", LastTransitionTime: stamp, Reason: "PodScheduled", Message: "Pod was successfully assigned to this node"})
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "Initialized", Status: "True", LastTransitionTime: stamp, Reason: "PodInitialized", Message: "Pod initialization completed"})
	readyReason := "ContainersNotReady"
	readyMessage := "containers are not ready"
	if ready {
		readyReason = "ContainersReady"
		readyMessage = "containers are ready"
	}
	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "Ready", Status: readyStatus, LastTransitionTime: stamp, Reason: readyReason, Message: readyMessage})
	status.Conditions = setPodCondition(status.Conditions, PodCondition{Type: "ContainersReady", Status: readyStatus, LastTransitionTime: stamp, Reason: readyReason, Message: readyMessage})
	container := ContainerStatus{Name: "", Ready: ready, RestartCount: restartCount, State: ContainerState{}}
	if len(pod.Spec.Containers) > 0 {
		container.Name = pod.Spec.Containers[0].Name
		container.Image = pod.Spec.Containers[0].Image
	}
	switch {
	case running:
		container.State.Running = &ContainerStateRunning{StartedAt: stamp}
	case phase == "Succeeded" || phase == "Failed":
		terminatedReason := "Error"
		if phase == "Succeeded" {
			terminatedReason = "Completed"
		}
		terminated := &ContainerStateTerminated{Reason: terminatedReason, Message: message, FinishedAt: stamp}
		if inspection != nil {
			if inspection.ExitCode != nil {
				terminated.ExitCode = *inspection.ExitCode
			}
			if inspection.StartedAt != nil {
				terminated.StartedAt = metav1.NewTime(inspection.StartedAt.UTC())
			}
			if inspection.FinishedAt != nil {
				terminated.FinishedAt = metav1.NewTime(inspection.FinishedAt.UTC())
			}
			if inspection.TerminationReason != "" || inspection.TerminationSignal != "" {
				terminationDetail := inspection.TerminationReason
				if inspection.TerminationSignal != "" {
					if terminationDetail != "" {
						terminationDetail += "; "
					}
					terminationDetail += "signal=" + inspection.TerminationSignal
				}
				if terminationDetail != "" {
					terminated.Message += "; Macker termination: " + terminationDetail
				}
			}
		}
		container.State.Terminated = terminated
	default:
		waitingReason := "ContainerCreating"
		switch reason {
		case "MacletUnsupportedPod":
			waitingReason = "CreateContainerConfigError"
		case "MacletMackerLaunchFailed":
			waitingReason = "RunContainerError"
		}
		container.State.Waiting = &ContainerStateWaiting{Reason: waitingReason, Message: message}
	}
	status.ContainerStatuses = []ContainerStatus{container}
	return status
}

func updatePodStatus(ctx context.Context, client *APIClient, pod *Pod, desired PodStatus) (*Pod, error) {
	current := pod
	for attempt := 0; attempt < 5; attempt++ {
		payload := map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":            current.ObjectMeta.Name,
				"namespace":       current.ObjectMeta.Namespace,
				"uid":             current.ObjectMeta.UID,
				"resourceVersion": current.ObjectMeta.ResourceVersion,
				// NodeRestriction compares the full object metadata during a
				// status update. Preserve the labels/annotations owned by the
				// user and admission controllers instead of submitting them as
				// an empty metadata map.
				"labels":      current.ObjectMeta.Labels,
				"annotations": current.ObjectMeta.Annotations,
			},
			"status": desired,
		}
		body, err := client.PutJSON(ctx, podStatusPath(*current), payload)
		if err == nil {
			var updated Pod
			if decodeErr := json.Unmarshal(body, &updated); decodeErr != nil {
				return nil, fmt.Errorf("decode Pod status response: %w", decodeErr)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(err, &conflict) || conflict.Code != 409 || attempt == 4 {
			return nil, fmt.Errorf("update Pod %s/%s status: %w", current.ObjectMeta.Namespace, current.ObjectMeta.Name, err)
		}
		body, getErr := client.Get(ctx, podPath(*current))
		if getErr != nil {
			return nil, fmt.Errorf("refresh Pod after status conflict: %w", getErr)
		}
		var refreshed Pod
		if getErr := json.Unmarshal(body, &refreshed); getErr != nil {
			return nil, fmt.Errorf("decode Pod after status conflict: %w", getErr)
		}
		current = &refreshed
		desired = desiredPodStatus(*current, desired.HostIP, string(desired.Phase), desired.PodIP, desired.Reason, desired.Message, desired.Phase == "Running", firstContainerRestartCount(desired))
	}
	return nil, errors.New("Pod status update retry limit exceeded")
}

func firstContainerRestartCount(status PodStatus) int32 {
	if len(status.ContainerStatuses) == 0 {
		return 0
	}
	return status.ContainerStatuses[0].RestartCount
}

func podHasNativeLabel(pod Pod) bool {
	return pod.ObjectMeta.Labels[nativeWorkloadLabelKey] == nativeWorkloadLabelValue
}

func (m *workloadManager) updateStatus(ctx context.Context, client *APIClient, pod *Pod, phase, ip, reason, message string, running bool, restartCount int32, termination ...*mackerInspection) error {
	desired := desiredPodStatus(*pod, m.nodeIP, phase, ip, reason, message, running, restartCount, termination...)
	updated, err := updatePodStatus(ctx, client, pod, desired)
	if err != nil {
		return err
	}
	*pod = *updated
	return nil
}
