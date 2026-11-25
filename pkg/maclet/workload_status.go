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
	// PodStatus.reason carries maclet's own named reason while a Pod sits Pending.
	// The phase stays canonical (Running, Pending, Succeeded, Failed), and for
	// those states the reason is left empty. It is filled in only for Pending,
	// because that is the state where "why is this Pod stuck" has to be answered,
	// and Kubernetes gives a node agent no other machine-readable field for it:
	// the kubelet vocabulary in the container waiting state is a fixed small set,
	// so a client that branches on reasons would otherwise have to parse message
	// prose. This is the field `kubectl get pod -o jsonpath={.status.reason}`
	// reads, and it is the one KUBERNETES-PARITY.md promises.
	status.Reason = ""
	if phase == "Pending" && reason != "" {
		status.Reason = reason
	}
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
		case "MacletUnsupportedPod", "MacletMissingNativeLabel":
			// "This manifest is not runnable on this node class" is a configuration
			// problem. ContainerCreating would claim maclet is still working on it.
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

const (
	// unlabelledPendingReason and unlabelledPendingMessage explain a Pod that a
	// scheduler placed on a maclet node but maclet will never run. Kubernetes
	// has no canonical reason for "this node agent ignores you", and silence is
	// how a portable manifest becomes an indefinitely Pending Pod, so maclet
	// states the deviation on the Pod instead of leaving it undiscoverable.
	unlabelledPendingReason  = "MacletMissingNativeLabel"
	unlabelledPendingMessage = "maclet only supervises Pods labelled " + nativeWorkloadLabelKey + "=" + nativeWorkloadLabelValue + "; add that label, or schedule this Pod on a Linux node (see KUBERNETES-PARITY.md)"
)

// explainUnlabelledPod reports why an unlabelled Pod stays Pending. It writes
// only while the explanation is missing, because maclet reconciles every few
// seconds and an unlabelled Pod must not become API-server churn.
func (m *workloadManager) explainUnlabelledPod(ctx context.Context, client *APIClient, pod *Pod) {
	if phase := string(pod.Status.Phase); phase != "" && phase != "Pending" {
		return
	}
	if pod.Status.Message == unlabelledPendingMessage {
		return
	}
	_ = m.updateStatus(ctx, client, pod, "Pending", pod.Status.PodIP, unlabelledPendingReason, unlabelledPendingMessage, false, 0)
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
