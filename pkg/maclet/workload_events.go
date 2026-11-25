package maclet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maclet writes Events because its refusals were otherwise invisible in the one
// place operators look first. `kubectl describe pod` on a maclet node showed only
// what the scheduler had written: the node's actual account — "this manifest asks
// for a read-only mount this OS cannot enforce" — lived in `.status.message` and in
// the maclet log, so an operator reading a `CreateContainerConfigError` had no
// reason string to grep and no path to the contract that explains it. That cost
// home-ops three re-derivations of maclet's semantics in one session, which is the
// bug this file fixes.
//
// The dedup discipline is the whole design. Reconciliation runs every few seconds
// and rewrites the same status on every pass, so an unthrottled writer produces a
// Pod whose Event list buries the one line that matters. maclet therefore keys an
// Event to the *state* it is reporting, names it deterministically, writes it once,
// and then stays quiet until either the state changes or the cooldown expires.
//
// What a node is actually allowed to do shapes that. NodeRestriction grants a node
// create and patch on events but not get, so maclet can never read an Event back to
// increment it (measured: a GET returns 403 for system:node:icarus-2.local). The
// count is therefore maclet's own tally of how many times it has reported a state,
// merged in with a patch, and a repeat POST that collides with the object a previous
// maclet process filed is a merge rather than a re-read.

const (
	// macletEventRepeatInterval is how long maclet stays quiet about a Pod state it
	// has already reported. A state that persists past this window is re-touched
	// rather than duplicated, so `describe` shows a live age and a rising count
	// instead of a fresh Event every reconcile.
	macletEventRepeatInterval = 30 * time.Minute

	// macletEventDetailLimit keeps a long Macker failure detail from crowding the
	// Event list. The head of the message is what names the problem, so the tail is
	// what gets cut.
	macletEventDetailLimit = 900

	// macletParityReference is why every maclet Event carries a pointer: a reason
	// string like MacletUnsupportedPod only means something next to the contract
	// that produced it, and the contract is prose in a file, not a field.
	macletParityReference = "Darwin node semantics: see KUBERNETES-PARITY.md, section \"How a refusal surfaces\", in github.com/initialed85/maclet."
)

// podEventMemory is what maclet remembers about the Event it last wrote for a Pod.
// It is process-local on purpose: the authoritative record is the Event object in
// the API server, so a maclet restart re-derives the state and finds (or creates)
// the same deterministic object instead of needing its own history.
type podEventMemory struct {
	key string
	at  time.Time
	// count is how many times maclet has reported this state. It is tracked locally
	// because a node author is not allowed to read an Event back -- NodeRestriction
	// grants create and patch on events, and a GET is refused with 403 -- so the count
	// in the object is whatever the last writer said, and maclet's own tally is the
	// only number it can offer.
	count int32
}

// podEventStateKey identifies the *state* an Event reports: which Pod, the phase,
// maclet's own reason, and the detail that goes with them. Two Pods in the same state
// produce different Events, and one Pod in the same state produces one Event.
func podEventStateKey(pod Pod, phase, reason, message string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, string(pod.ObjectMeta.UID),
		phase, reason, message,
	}, "\x00")))
	return string(pod.ObjectMeta.UID) + "\x00" + hex.EncodeToString(digest[:])[:16]
}

// podEventName is the deterministic object name for a Pod state. It is a function of
// the Pod and the state rather than a timestamp, which is what makes the write
// idempotent: maclet can POST the same Event again after a restart and either get it
// created once or discover the existing one and bump its count.
func podEventName(pod Pod, phase, reason, message string) string {
	digest := sha256.Sum256([]byte(podEventStateKey(pod, phase, reason, message)))
	hash := hex.EncodeToString(digest[:])[:10]
	// The Pod's own name makes the Event list readable. The namespace is not part of
	// it: the Event already lives in that namespace, and including both produced names
	// whose head was a truncated fragment of a namespace.
	podKey := sanitizeNamePart(strings.ToLower(pod.ObjectMeta.Name))
	if len(podKey) > 32 {
		podKey = sanitizeNamePart(podKey[len(podKey)-32:])
	}
	name := "maclet." + sanitizeNamePart(strings.ToLower(phase)) + "." + sanitizeNamePart(shortEventReason(reason))
	if podKey != "" {
		name += "." + podKey
	}
	// The hash is the only thing separating two states of one Pod, so it is kept and
	// the readable part gives way. Truncating the other way -- which is what this used
	// to do -- collapsed "Macker started the trusted native workload" and "Macker is
	// running the trusted native workload" onto one object name on a long Pod name, and
	// the second report silently merged over the first.
	if len(name)+1+len(hash) > 63 {
		name = name[:63-1-len(hash)]
	}
	name += "." + hash
	// A Kubernetes object name must end alphanumeric; the hash tail guarantees that,
	// and the sanitizer trims whatever the truncation exposed.
	return sanitizeNamePart(name)
}

// sanitizeNamePart makes a string usable as a DNS-1123 label component: only
// alphanumerics, '.' and '-', and never a leading or trailing separator.
func sanitizeNamePart(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), ".-")
}

// shortEventReason keeps an Event name readable. The full reason is always in the
// Event's own reason field, which is what clients branch on.
func shortEventReason(reason string) string {
	switch reason {
	case "":
		return "status"
	case "MacletUnsupportedPod":
		return "unsupported"
	case "MacletMissingNativeLabel":
		return "unlabelled"
	case "MacletMackerLaunchFailed":
		return "launch-failed"
	default:
		return strings.ToLower(strings.TrimPrefix(reason, "Maclet"))
	}
}

// podEventType classifies the Event the way Kubernetes does: a state the node could
// not reach is a Warning, a state it achieved is Normal.
func podEventType(phase, reason string) string {
	switch phase {
	case "Running", "Succeeded":
		return "Normal"
	case "Pending", "Failed", "Unknown":
		return "Warning"
	}
	if strings.HasPrefix(reason, "Maclet") {
		return "Warning"
	}
	return "Normal"
}

// recordPodEvent writes maclet's own account of why a Pod is in the state it is in,
// deduplicated against what it has already said. It never fails the status write it
// follows: an Event is a report, and a report that cannot be filed is not a reason to
// stop supervising the workload.
func (m *workloadManager) recordPodEvent(ctx context.Context, client *APIClient, pod *Pod, phase, reason, message string) {
	if client == nil {
		return
	}
	if phase == "Running" && reason == "" && message == "" {
		// The ordinary steady state, which kubelet does not narrate either.
		return
	}
	key := podEventStateKey(*pod, phase, reason, message)
	podKey := pod.ObjectMeta.Namespace + "/" + pod.ObjectMeta.Name

	m.eventMu.Lock()
	memory, seen := m.podEvents[podKey]
	if seen && memory.key == key && time.Since(memory.at) < macletEventRepeatInterval {
		m.eventMu.Unlock()
		return
	}
	changed := !seen || memory.key != key
	count := int32(1)
	if !changed {
		// A state that is still true an interval later: same Event, said again.
		count = memory.count + 1
	}
	m.podEvents[podKey] = podEventMemory{key: key, at: time.Now(), count: count}
	m.eventMu.Unlock()

	event := m.buildPodEvent(pod, phase, reason, message, count)
	emit := m.emitEvent
	if emit == nil {
		emit = postPodEvent
	}
	if err := emit(ctx, client, event, count == 1); err != nil {
		if eventWriteRefusedWhileDeleting(err) {
			// The memory is kept on purpose: this Pod's namespace is going away, so no
			// retry can land and the API server is already telling the user so. Logging
			// it once per pass would bury the reconciliation warnings that matter.
			if m.debug {
				log.Printf("debug: dropping Event for Pod %s, its namespace is terminating: %v", podKey, err)
			}
			return
		}
		// Forget the memory so the next pass retries rather than staying quiet about
		// something the API server never accepted.
		m.eventMu.Lock()
		delete(m.podEvents, podKey)
		m.eventMu.Unlock()
		log.Printf("maclet: could not record an Event for Pod %s: %v", podKey, err)
		return
	}
	if m.debug {
		log.Printf("debug: recorded Event %s/%s reason=%s for Pod %s", event.ObjectMeta.Namespace, event.ObjectMeta.Name, event.Reason, podKey)
	}
}

// eventWriteRefusedWhileDeleting reports whether an Event write failed because the
// namespace is already being torn down, which is a refusal no retry can change: the
// API server answers 403 "unable to create new content in namespace <x>" once a
// namespace is Terminating, and 404 once it is gone.
func eventWriteRefusedWhileDeleting(err error) bool {
	var apiErr *HTTPError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code == http.StatusNotFound {
		return true
	}
	return apiErr.Code == http.StatusForbidden && strings.Contains(apiErr.Body, "unable to create new content in namespace")
}

// buildPodEvent assembles the object maclet files. The namespace, host, and
// reporting identity are the three fields NodeRestriction checks for a node author,
// so each is taken from the Pod itself rather than from configuration that could
// disagree with it. count is how many times maclet has reported this state.
func (m *workloadManager) buildPodEvent(pod *Pod, phase, reason, message string, count int32) *Event {
	now := metav1.NewTime(time.Now().UTC())
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		nodeName = m.nodeIP
	}
	eventReason := reason
	if eventReason == "" {
		switch phase {
		case "Running":
			eventReason = "MacletWorkloadRunning"
		case "Succeeded":
			eventReason = "MacletWorkloadSucceeded"
		case "Failed":
			eventReason = "MacletWorkloadFailed"
		default:
			eventReason = "MacletStatusUpdate"
		}
	}
	detail := strings.TrimSpace(message)
	if len(detail) > macletEventDetailLimit {
		detail = detail[:macletEventDetailLimit] + " …"
	}
	combined := detail
	if combined == "" {
		combined = fmt.Sprintf("maclet reports Pod phase %q on node %s.", phase, nodeName)
	} else {
		combined = combined + " " + macletParityReference
	}
	event := &Event{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Event"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podEventName(*pod, phase, reason, message),
			Namespace: pod.ObjectMeta.Namespace,
		},
		InvolvedObject: coreObjectReference(pod),
		Reason:         eventReason,
		Message:        combined,
		Source: EventSource{
			Component: "maclet",
			Host:      nodeName,
		},
		ReportingController: "maclet",
		ReportingInstance:   nodeName,
		Type:                podEventType(phase, reason),
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               count,
	}
	return event
}

// coreObjectReference points the Event at the Pod, in the namespace the Pod lives in.
// NodeRestriction requires an event authoring node to name a namespace equal to the
// involved object's, so it is taken from there rather than composed.
func coreObjectReference(pod *Pod) ObjectReference {
	return ObjectReference{
		Kind:       "Pod",
		Namespace:  pod.ObjectMeta.Namespace,
		Name:       pod.ObjectMeta.Name,
		UID:        pod.ObjectMeta.UID,
		APIVersion: "v1",
	}
}

// postPodEvent files the Event. The first report of a state creates it; a later
// report of the same state merges into the object that already exists, so the Event
// list keeps one line per state with a rising count -- the deduplication home-ops
// asked for. create is derived from maclet's count rather than from a read, because a
// node author cannot read Events back (see patchPodEvent).
func postPodEvent(ctx context.Context, client *APIClient, event *Event, create bool) error {
	path := "/api/v1/namespaces/" + event.ObjectMeta.Namespace + "/events"
	if !create {
		return patchPodEvent(ctx, client, event)
	}
	if _, err := client.PostJSON(ctx, path, event); err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Code == 409 {
			// The object exists -- a previous maclet process wrote this same state.
			// Merge into it rather than reporting a failure forever.
			return patchPodEvent(ctx, client, event)
		}
		return err
	}
	return nil
}

// patchPodEvent merges a repeat report into the Event already stored. It cannot read
// the object first: NodeRestriction grants a node create and patch on events but not
// get, and a GET is answered with 403 (measured on icarus-2 against the live API
// server), which is why maclet keeps its own tally and sends it. A patch denied for
// lack of permission is not a failure worth logging every reconcile -- the state was
// reported once, and the object carries it -- so it is treated as done.
func patchPodEvent(ctx context.Context, client *APIClient, event *Event) error {
	namePath := "/api/v1/namespaces/" + event.ObjectMeta.Namespace + "/events/" + event.ObjectMeta.Name
	// No resourceVersion: there is no way to read the current one, and last-writer
	// wins is the right semantics for a counter on a report.
	payload := map[string]any{
		"metadata":      map[string]any{"name": event.ObjectMeta.Name, "namespace": event.ObjectMeta.Namespace},
		"reason":        event.Reason,
		"message":       event.Message,
		"type":          event.Type,
		"count":         event.Count,
		"lastTimestamp": event.LastTimestamp.Format(time.RFC3339Nano),
	}
	if _, err := client.PatchJSON(ctx, namePath, payload); err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			switch httpErr.Code {
			case 403:
				return nil
			case 404:
				// Expired or deleted: file it again as new.
				_, postErr := client.PostJSON(ctx, "/api/v1/namespaces/"+event.ObjectMeta.Namespace+"/events", event)
				return postErr
			}
		}
		return err
	}
	return nil
}
