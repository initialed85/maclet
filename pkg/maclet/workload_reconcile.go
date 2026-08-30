package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (m *workloadManager) stopContainer(workload *managedWorkload) error {
	if workload == nil || workload.ContainerName == "" {
		return nil
	}
	var cleanupErrors []error
	if _, err := m.mackerOutput("stop", workload.ContainerName); err != nil && !ignorableMackerCleanupError(err) {
		cleanupErrors = append(cleanupErrors, err)
	}
	if _, err := m.mackerOutput("rm", "--force", workload.ContainerName); err != nil && !ignorableMackerCleanupError(err) {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func ignorableMackerCleanupError(err error) bool {
	if err == nil {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "was not found") || strings.Contains(message, "not a detached workload")
}

func (m *workloadManager) removeWorkload(workload *managedWorkload) error {
	if workload == nil {
		return nil
	}
	var cleanupErrors []error
	if err := m.stopContainer(workload); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if workload.IP != "" && m.network != nil {
		if err := m.network.removeWorkloadIP(workload.IP); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func listAssignedPods(ctx context.Context, client *APIClient, nodeName string) ([]Pod, error) {
	query := url.Values{"fieldSelector": []string{"spec.nodeName=" + nodeName}}
	body, err := client.Get(ctx, "/api/v1/pods?"+query.Encode())
	if err != nil {
		return nil, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, fmt.Errorf("decode assigned PodList: %w", err)
	}
	return pods.Items, nil
}

func (m *workloadManager) reconcile(ctx context.Context, client *APIClient, pods []Pod) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.network == nil {
		return nil
	}
	used := make(map[string]bool)
	for _, pod := range pods {
		if pod.Status.PodIP != "" {
			used[pod.Status.PodIP] = true
		}
	}
	seen := make(map[string]bool)
	var reconcileErrors []error
	for index := range pods {
		pod := &pods[index]
		uid := string(pod.ObjectMeta.UID)
		if uid == "" {
			uid = pod.ObjectMeta.Namespace + "/" + pod.ObjectMeta.Name
		}
		if pod.ObjectMeta.DeletionTimestamp != nil {
			seen[uid] = true
			workload := m.workloads[uid]
			if workload == nil {
				workload = &managedWorkload{
					UID:              uid,
					Namespace:        pod.ObjectMeta.Namespace,
					Name:             pod.ObjectMeta.Name,
					PodContainerName: firstPodContainerName(*pod),
					ContainerName:    workloadContainerName(*pod),
					IP:               pod.Status.PodIP,
				}
			}
			if err := m.removeWorkload(workload); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("remove deleted Pod %s/%s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
				m.workloads[uid] = workload
				continue
			}
			delete(m.workloads, uid)
			if err := m.persistJournalLocked(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist deleted Pod %s/%s cleanup: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			}
			// A real kubelet reports the workload terminal before the API server
			// finishes a graceful Pod deletion. Without that acknowledgement,
			// deleting a native Pod can leave it stuck in Terminating forever.
			if err := m.updateStatus(ctx, client, pod, "Failed", pod.Status.PodIP, "MacletWorkloadDeleted", "maclet stopped the native workload for Pod deletion", false, workload.RestartCount); err != nil {
				var apiErr *HTTPError
				if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
					reconcileErrors = append(reconcileErrors, err)
				}
			}
			continue
		}
		if !podHasNativeLabel(*pod) {
			continue
		}
		seen[uid] = true
		if len(pod.Spec.Containers) != 1 {
			err := fmt.Errorf("Pod %s/%s must declare exactly one container; maclet does not support sidecars yet", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name)
			// Configuration errors are Pending, not terminal Failed: a ReplicaSet
			// replaces terminal Pods, which can create an unbounded stream of
			// identical Pods when a native feature is unsupported.
			if statusErr := m.updateStatus(ctx, client, pod, "Pending", pod.Status.PodIP, "MacletUnsupportedPod", err.Error(), false, 0); statusErr != nil {
				reconcileErrors = append(reconcileErrors, statusErr)
			}
			continue
		}
		managed := m.workloads[uid]
		journalChanged := false
		if managed == nil {
			managed = &managedWorkload{UID: uid, PodContainerName: firstPodContainerName(*pod), ContainerName: workloadContainerName(*pod)}
			if existing := pod.Status.ContainerStatuses; len(existing) > 0 {
				managed.RestartCount = existing[0].RestartCount
			}
			m.workloads[uid] = managed
			journalChanged = true
		}
		if managed.Namespace != pod.ObjectMeta.Namespace || managed.Name != pod.ObjectMeta.Name || managed.PodContainerName != firstPodContainerName(*pod) {
			managed.Namespace = pod.ObjectMeta.Namespace
			managed.Name = pod.ObjectMeta.Name
			managed.PodContainerName = firstPodContainerName(*pod)
			journalChanged = true
		}
		if journalChanged {
			if err := m.persistJournalLocked(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
		}
		ip := pod.Status.PodIP
		if ip == "" {
			allocated, err := m.network.firstAvailableWorkloadIP(used)
			if err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("allocate address for Pod %s/%s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
				_ = m.updateStatus(ctx, client, pod, "Pending", "", "MacletAddressAllocationFailed", err.Error(), false, managed.RestartCount)
				continue
			}
			ip = allocated
			used[ip] = true
		}
		if err := m.network.validateWorkloadAddress(ip); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("Pod %s/%s has invalid PodIP %s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, ip, err))
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletInvalidPodIP", err.Error(), false, managed.RestartCount)
			continue
		}
		if managed.IP != "" && managed.IP != ip {
			if err := m.network.removeWorkloadIP(managed.IP); err != nil {
				reconcileErrors = append(reconcileErrors, err)
				continue
			}
		}
		if managed.IP != ip {
			managed.IP = ip
			if err := m.persistJournalLocked(); err != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s address: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
		}
		if err := m.network.addWorkloadIP(ip); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("add address for Pod %s/%s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletNetworkSetupFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if (pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed") && podRestartPolicy(*pod) != "Always" {
			continue
		}
		if !managed.RetryAfter.IsZero() && time.Now().Before(managed.RetryAfter) {
			continue
		}
		status, found, err := m.containerStatus(managed.ContainerName)
		if err != nil {
			reconcileErrors = append(reconcileErrors, err)
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerUnavailable", err.Error(), false, managed.RestartCount)
			continue
		}
		if found && status == "running" {
			managed.RetryAfter = time.Time{}
			if err := m.updateStatus(ctx, client, pod, "Running", ip, "MacletWorkloadRunning", "Macker is running the trusted native workload", true, managed.RestartCount); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
			continue
		}
		if found && status != "running" {
			inspection, inspectionAvailable, inspectErr := m.containerInspection(managed.ContainerName)
			if inspectErr != nil {
				reconcileErrors = append(reconcileErrors, inspectErr)
				managed.RetryAfter = time.Now().Add(workloadRetryDelay)
				continue
			}
			if !inspectionAvailable {
				inspection = nil
			}
			if m.debug {
				log.Printf("debug: Macker container %s status=%s inspection=%+v", managed.ContainerName, status, inspection)
				if output, logsErr := m.mackerOutput("logs", managed.ContainerName); logsErr != nil {
					log.Printf("debug: Macker logs for %s unavailable: %v", managed.ContainerName, logsErr)
				} else if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
					log.Printf("debug: Macker logs for %s: %q", managed.ContainerName, trimmed)
				}
			}
			switch podRestartPolicy(*pod) {
			case "Never", "OnFailure":
				phase := "Succeeded"
				reason := "MacletWorkloadCompleted"
				message := "Macker stopped the workload"
				if podRestartPolicy(*pod) == "OnFailure" {
					phase = "Failed"
					reason = "MacletWorkloadExited"
					message = "Macker workload exited"
				}
				if inspection != nil && inspection.ExitCode != nil {
					if *inspection.ExitCode != 0 {
						phase = "Failed"
						reason = "MacletWorkloadFailed"
						message = fmt.Sprintf("Macker workload exited with code %d", *inspection.ExitCode)
					} else {
						phase = "Succeeded"
						reason = "MacletWorkloadCompleted"
						message = "Macker workload exited successfully"
					}
				} else if inspection == nil {
					if podRestartPolicy(*pod) == "Never" {
						message = "Macker stopped the workload; exit code is not available through the current Macker CLI"
					} else {
						message = "Macker workload exited; exit code is not available through the current Macker CLI"
					}
				}
				_ = m.stopContainer(managed)
				if err := m.updateStatus(ctx, client, pod, phase, ip, reason, message, false, managed.RestartCount, inspection); err != nil {
					reconcileErrors = append(reconcileErrors, err)
				}
				continue
			default:
				if err := m.stopContainer(managed); err != nil {
					reconcileErrors = append(reconcileErrors, err)
					managed.RetryAfter = time.Now().Add(workloadRetryDelay)
					continue
				}
				managed.RestartCount++
				if err := m.persistJournalLocked(); err != nil {
					reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s restart: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
					managed.RetryAfter = time.Now().Add(workloadRetryDelay)
					continue
				}
			}
		}
		args, err := m.runArgs(*pod, pod.Spec.Containers[0], managed)
		if err != nil {
			if m.debug {
				log.Printf("debug: cannot construct Macker invocation for %s/%s: %v", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
			}
			reconcileErrors = append(reconcileErrors, fmt.Errorf("prepare Macker workload %s/%s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			// Keep invalid configuration Pending so ReplicaSets do not treat the
			// Pod as dead and create another copy on every reconciliation.
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerLaunchFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if m.debug {
			log.Printf("debug: Macker invocation for %s/%s: %s", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, formatCommandArgs(args))
		}
		if _, err := m.mackerOutput(args...); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("start Macker workload %s/%s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerLaunchFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if err := m.waitForMackerRunning(managed.ContainerName); err != nil {
			if m.debug {
				log.Printf("debug: Macker startup failed for %s/%s: %v", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err)
			}
			_ = m.stopContainer(managed)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("Macker workload %s/%s did not start: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletMackerLaunchFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		if err := m.persistJournalLocked(); err != nil {
			if cleanupErr := m.removeWorkload(managed); cleanupErr != nil {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w (cleanup: %v)", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err, cleanupErr))
			} else {
				reconcileErrors = append(reconcileErrors, fmt.Errorf("persist workload %s/%s ownership: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, err))
			}
			managed.RetryAfter = time.Now().Add(workloadRetryDelay)
			_ = m.updateStatus(ctx, client, pod, "Pending", ip, "MacletOwnershipPersistFailed", err.Error(), false, managed.RestartCount)
			continue
		}
		managed.RetryAfter = time.Time{}
		if err := m.updateStatus(ctx, client, pod, "Running", ip, "MacletWorkloadRunning", "Macker started the trusted native workload", true, managed.RestartCount); err != nil {
			reconcileErrors = append(reconcileErrors, err)
		}
	}
	journalChanged := false
	for uid, workload := range m.workloads {
		if seen[uid] {
			continue
		}
		if err := m.removeWorkload(workload); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("remove stale maclet workload %s: %w", workload.ContainerName, err))
			continue
		}
		delete(m.workloads, uid)
		journalChanged = true
	}
	if journalChanged {
		if err := m.persistJournalLocked(); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("persist stale workload cleanup: %w", err))
		}
	}
	return errors.Join(reconcileErrors...)
}
