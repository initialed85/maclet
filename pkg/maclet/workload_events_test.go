package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// eventRecorder captures what maclet would have filed, and whether it was a new
// Event or a re-touch of one already reported.
type eventRecorder struct {
	mu      sync.Mutex
	events  []*Event
	creates bool
	err     error
}

func (r *eventRecorder) emit(ctx context.Context, client *APIClient, event *Event, create bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, event)
	if create {
		r.creates = true
	}
	return nil
}

func (r *eventRecorder) recorded() []*Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Event(nil), r.events...)
}

// serveEvent handles the Event writes maclet makes after a status update, so tests
// that assert on the status path can ignore them. It reports whether it served the
// request.
func serveEvent(response http.ResponseWriter, request *http.Request) bool {
	if !strings.Contains(request.URL.Path, "/events") {
		return false
	}
	if request.Method != http.MethodPost && request.Method != http.MethodGet && request.Method != http.MethodPut && request.Method != http.MethodPatch {
		return false
	}
	body, _ := io.ReadAll(request.Body)
	event := &Event{}
	if len(body) > 0 {
		_ = json.Unmarshal(body, event)
	}
	if event.ObjectMeta.Name == "" {
		event.ObjectMeta.Name = "maclet.generated"
	}
	if event.ObjectMeta.ResourceVersion == "" {
		event.ObjectMeta.ResourceVersion = "1"
	}
	event.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Event"}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(event)
	return true
}

func eventTestPod() *Pod {
	return &Pod{
		ObjectMeta: ObjectMeta{Namespace: "Demo_Namespace", Name: "hello", UID: types.UID("ABC-123-DEF")},
		Spec:       PodSpec{NodeName: "icarus-2.local"},
	}
}

func newEventTestManager() (*workloadManager, *eventRecorder) {
	manager := newWorkloadManager(nil, "/usr/local/bin/macker", "192.168.137.111")
	recorder := &eventRecorder{}
	manager.emitEvent = recorder.emit
	return manager, recorder
}

// TestRecordPodEventFilesEachStateOnce is the deduplication home-ops asked for:
// reconciliation rewrites the same status every pass, and must not turn that into an
// Event per pass.
func TestRecordPodEventFilesEachStateOnce(t *testing.T) {
	manager, recorder := newEventTestManager()
	ctx := context.Background()
	pod := eventTestPod()
	for i := 0; i < 5; i++ {
		manager.recordPodEvent(ctx, &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "readOnly volume is not enforceable here")
	}
	events := recorder.recorded()
	if len(events) != 1 {
		t.Fatalf("recorded %d Events for one persistent state, want 1: %v", len(events), events)
	}
	if !recorder.creates {
		t.Error("the first Event for a state should be created, not touched")
	}
	// A different state about the same Pod is worth saying.
	manager.recordPodEvent(ctx, &APIClient{}, pod, "Pending", "MacletMackerLaunchFailed", "macker run failed")
	if got := len(recorder.recorded()); got != 2 {
		t.Fatalf("recorded %d Events after the state changed, want 2", got)
	}
	// Returning to the first state is itself a change, so it is said once; saying it
	// again immediately is not.
	manager.recordPodEvent(ctx, &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "readOnly volume is not enforceable here")
	manager.recordPodEvent(ctx, &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "readOnly volume is not enforceable here")
	if got := len(recorder.recorded()); got != 3 {
		t.Fatalf("recorded %d Events, want the flip back counted once and the repeat suppressed", got)
	}
}

// TestRecordPodEventRetouchesAnExpiredState keeps a long-lived refusal visible as
// live without duplicating it: after the interval the same state is re-filed as a
// touch of the object maclet already wrote.
func TestRecordPodEventRetouchesAnExpiredState(t *testing.T) {
	manager, recorder := newEventTestManager()
	pod := eventTestPod()
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "no enforcement mechanism")
	manager.eventMu.Lock()
	memory := manager.podEvents["Demo_Namespace/hello"]
	memory.at = time.Now().Add(-macletEventRepeatInterval - time.Minute)
	manager.podEvents["Demo_Namespace/hello"] = memory
	manager.eventMu.Unlock()
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "no enforcement mechanism")
	events := recorder.recorded()
	if len(events) != 2 {
		t.Fatalf("recorded %d Events, want the expired state re-touched", len(events))
	}
	if events[1].Count != 2 {
		t.Errorf("a repeat report should carry count 2, got %d (maclet tracks the tally itself because a node cannot read an Event back)", events[1].Count)
	}
}

// TestPodEventPointsAtTheContract is the reason the feature exists: an operator
// reading `kubectl describe pod` must be able to get from the line to the semantics
// that produced it.
func TestPodEventPointsAtTheContract(t *testing.T) {
	manager, recorder := newEventTestManager()
	pod := eventTestPod()
	message := "volume mount \"/ms/\" requests readOnly on \"/Users/x/config\", which macOS cannot enforce. Mount a directory and point the mount at it."
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", message)
	events := recorder.recorded()
	if len(events) != 1 {
		t.Fatalf("recorded %d Events, want 1", len(events))
	}
	event := events[0]
	if event.Type != "Warning" {
		t.Errorf("type = %q, want Warning for a refusal", event.Type)
	}
	if event.Reason != "MacletUnsupportedPod" {
		t.Errorf("reason = %q, want MacletUnsupportedPod so clients can branch on it", event.Reason)
	}
	if !strings.Contains(event.Message, "KUBERNETES-PARITY.md") {
		t.Errorf("message %q does not point at the parity contract", event.Message)
	}
	if !strings.Contains(event.Message, "macOS cannot enforce") {
		t.Errorf("message %q dropped maclet's own explanation", event.Message)
	}
	if event.ObjectMeta.Namespace != pod.ObjectMeta.Namespace {
		t.Errorf("Event namespace %q must equal the Pod namespace %q", event.ObjectMeta.Namespace, pod.ObjectMeta.Namespace)
	}
	if event.InvolvedObject.Name != "hello" || event.InvolvedObject.Kind != "Pod" || event.InvolvedObject.UID != pod.ObjectMeta.UID {
		t.Errorf("involvedObject = %+v, want the Pod this describes", event.InvolvedObject)
	}
	if event.Source.Host != "icarus-2.local" || event.Source.Component != "maclet" {
		t.Errorf("source = %+v, want maclet on the Pod's node", event.Source)
	}
	if event.ReportingInstance != "icarus-2.local" || event.ReportingController != "maclet" {
		t.Errorf("reporting identity = %q/%q, want maclet/icarus-2.local (NodeRestriction checks this)", event.ReportingController, event.ReportingInstance)
	}
	// The name has to survive both DNS-1123 and being read by a human.
	if len(event.ObjectMeta.Name) > 63 || strings.HasPrefix(event.ObjectMeta.Name, ".") {
		t.Errorf("name %q is not a usable object name", event.ObjectMeta.Name)
	}
	if !strings.HasPrefix(event.ObjectMeta.Name, "maclet.pending.unsupported.hello.") {
		// The Pod's name is in the Event name so `describe` reads as a sentence. The
		// namespace is not, because the Event already lives in that namespace, and
		// underscores are not legal in a Kubernetes name.
		t.Errorf("name %q does not read as a maclet Event", event.ObjectMeta.Name)
	}
	if event.Count != 1 || event.FirstTimestamp.IsZero() || event.LastTimestamp.IsZero() {
		t.Errorf("count/timestamps = %d/%v/%v, want a first occurrence", event.Count, event.FirstTimestamp, event.LastTimestamp)
	}
}

// TestPodEventNamesAreStableAcrossRestarts is what makes the write idempotent: a
// restarted maclet must find the object it already filed rather than duplicating it.
func TestPodEventNamesAreStableAcrossRestarts(t *testing.T) {
	pod := eventTestPod()
	first := podEventName(*pod, "Pending", "MacletUnsupportedPod", "same message")
	second := podEventName(*pod, "Pending", "MacletUnsupportedPod", "same message")
	if first != second {
		t.Fatalf("name changed for the same state: %q vs %q", first, second)
	}
	other := podEventName(*pod, "Pending", "MacletUnsupportedPod", "different message")
	if other == first {
		t.Fatal("two different states collapsed onto one Event name")
	}
	otherPod := eventTestPod()
	otherPod.ObjectMeta.Name = "other"
	if podEventName(*otherPod, "Pending", "MacletUnsupportedPod", "same message") == first {
		t.Fatal("two Pods collapsed onto one Event name")
	}
	// The case the friendly fixture missed, found on the node: a Deployment-managed Pod
	// name plus the "maclet.<phase>.<reason>." prefix overflows 63 characters, and
	// maclet's two startup reports for one workload -- "Macker started the trusted
	// native workload" and "Macker is running the trusted native workload" -- landed on
	// the same name, so the second merged over the first and one line disappeared.
	long := eventTestPod()
	long.ObjectMeta.Name = "woodpecker-agent-darwin-565996794d-httdd"
	started := podEventName(*long, "Running", "MacletWorkloadRunning", "Macker started the trusted native workload")
	running := podEventName(*long, "Running", "MacletWorkloadRunning", "Macker is running the trusted native workload")
	if started == running {
		t.Fatalf("two states of one long-named Pod collapsed onto %q", started)
	}
	for _, name := range []string{started, running} {
		if len(name) > 63 || name != sanitizeNamePart(name) {
			t.Errorf("name %q is not a usable Kubernetes object name", name)
		}
		// The tail is the hash, not a fragment of the readable part.
		if last := strings.Split(name, ".")[len(strings.Split(name, "."))-1]; len(last) != 10 {
			t.Errorf("name %q lost its distinguishing hash (tail %q)", name, last)
		}
	}
	for _, name := range []string{first, other, podEventName(*otherPod, "Pending", "MacletMackerLaunchFailed", "x")} {
		if len(name) > 63 || name != sanitizeNamePart(name) {
			t.Errorf("name %q is not a usable Kubernetes object name", name)
		}
	}
}

// TestRecordPodEventSkipsTheSteadyState keeps the useful signal: maclet does not
// narrate an ordinary running Pod.
func TestRecordPodEventSkipsTheSteadyState(t *testing.T) {
	manager, recorder := newEventTestManager()
	pod := eventTestPod()
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Running", "", "")
	if events := recorder.recorded(); len(events) != 0 {
		t.Fatalf("recorded %d Events for an ordinary running Pod, want none", len(events))
	}
	// A running Pod with something to say is still worth one line.
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Running", "MacletWorkloadRunning", "serving on :18080")
	if events := recorder.recorded(); len(events) != 1 || events[0].Type != "Normal" {
		t.Fatalf("recorded %+v, want one Normal Event", events)
	}
}

// TestRecordPodEventRetriesAfterAFailure makes a reporting hiccup temporary: maclet
// must not decide it already said something the API server never accepted.
func TestRecordPodEventRetriesAfterAFailure(t *testing.T) {
	manager, recorder := newEventTestManager()
	recorder.err = errors.New("the API server said no")
	pod := eventTestPod()
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "refused")
	if len(recorder.recorded()) != 0 {
		t.Fatal("a failed emit should record nothing")
	}
	recorder.err = nil
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "refused")
	if len(recorder.recorded()) != 1 {
		t.Fatal("maclet should retry an Event the API server never took")
	}
}

// TestRecordPodEventStaysQuietWhenTheNamespaceIsGoingAway covers the noise a long
// teardown produced on a live node: maclet retried an Event into a Terminating
// namespace every pass, warning about a refusal that no retry can change. The API
// server is already reporting that condition, so maclet tries once per state and then
// goes quiet, while an ordinary failure keeps retrying.
func TestRecordPodEventStaysQuietWhenTheNamespaceIsGoingAway(t *testing.T) {
	manager, _ := newEventTestManager()
	pod := eventTestPod()
	attempts := 0
	failWith := func(err error) func(context.Context, *APIClient, *Event, bool) error {
		return func(context.Context, *APIClient, *Event, bool) error {
			attempts++
			return err
		}
	}
	terminating := &HTTPError{Code: http.StatusForbidden, Body: `{"message":"events \"maclet.failed.unsupported.demo\" is forbidden: unable to create new content in namespace maclet-lifecycle-1795603267"}`}
	manager.emitEvent = failWith(terminating)
	for i := 0; i < 4; i++ {
		manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletUnsupportedPod", "refused")
	}
	if attempts != 1 {
		t.Fatalf("tried the Event write %d times against a terminating namespace, want 1", attempts)
	}
	// A different state about the same Pod is a different thing to say, so it gets its
	// own single attempt rather than being swallowed by the suppression above.
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Failed", "MacletMackerLaunchFailed", "macker run failed")
	if attempts != 2 {
		t.Fatalf("tried %d times after the state changed, want 2", attempts)
	}
	// A transient failure is not suppressed: it retries until it lands.
	attempts = 0
	manager.emitEvent = failWith(&HTTPError{Code: http.StatusInternalServerError, Body: "boom"})
	for i := 0; i < 3; i++ {
		manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletVolumeHostPathMissing", "hostPath does not exist")
	}
	if attempts != 3 {
		t.Fatalf("tried %d times against a transient failure, want one per pass", attempts)
	}
}

// TestPostPodEventMergesIntoAnExistingObject covers the restart path: a deterministic
// name means a second maclet process collides with the Event it already filed, and the
// answer is to merge into that object rather than to fail. It also pins the permission
// boundary measured on the live node -- a node author may patch an Event but may not
// read one, so a denied patch is treated as "already reported" rather than retried
// forever.
func TestPostPodEventMergesIntoAnExistingObject(t *testing.T) {
	for _, tc := range []struct {
		name        string
		patchCode   int
		wantErr     bool
		wantCount   int32
		wantMessage string
		wantVerbs   []string
	}{
		{
			name:        "patch merges",
			patchCode:   http.StatusOK,
			wantCount:   2,
			wantMessage: "new detail",
			wantVerbs:   []string{"POST /api/v1/namespaces/ns/events", "PATCH /api/v1/namespaces/ns/events/maclet.Pending.unsupported.abc"},
		},
		{
			// The state was reported once already, so a denied merge leaves the original
			// line intact and is not something to retry on every reconcile.
			name:        "patch forbidden is not a failure",
			patchCode:   http.StatusForbidden,
			wantCount:   1,
			wantMessage: "old detail",
			wantVerbs:   []string{"POST /api/v1/namespaces/ns/events", "PATCH /api/v1/namespaces/ns/events/maclet.Pending.unsupported.abc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			stored := &Event{
				ObjectMeta:     metav1.ObjectMeta{Name: "maclet.Pending.unsupported.abc", Namespace: "ns", ResourceVersion: "9"},
				InvolvedObject: ObjectReference{Kind: "Pod", Namespace: "ns", Name: "hello"},
				Reason:         "MacletUnsupportedPod",
				Message:        "old detail",
				Type:           "Warning",
				Count:          1,
			}
			var methods []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				methods = append(methods, r.Method+" "+r.URL.Path)
				mu.Unlock()
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/ns/events":
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"AlreadyExists","code":409}`)
				case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/namespaces/ns/events/maclet.Pending.unsupported.abc":
					if tc.patchCode != http.StatusOK {
						w.WriteHeader(tc.patchCode)
						fmt.Fprintf(w, `{"kind":"Status","status":"Failure","code":%d}`, tc.patchCode)
						return
					}
					var patch struct {
						Count   int32  `json:"count"`
						Message string `json:"message"`
					}
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Fatalf("decode PATCH body: %v", err)
					}
					mu.Lock()
					stored.Count = patch.Count
					stored.Message = patch.Message
					mu.Unlock()
					body, _ := json.Marshal(stored)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusTeapot)
				}
			}))
			defer server.Close()
			client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
			if err != nil {
				t.Fatalf("build client: %v", err)
			}
			event := &Event{
				ObjectMeta: metav1.ObjectMeta{Name: "maclet.Pending.unsupported.abc", Namespace: "ns"},
				Reason:     "MacletUnsupportedPod",
				Message:    "new detail",
				Type:       "Warning",
				Count:      2,
			}
			err = postPodEvent(context.Background(), client, event, true)
			if tc.wantErr && err == nil {
				t.Fatal("postPodEvent succeeded, want a failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("postPodEvent: %v (requests %v)", err, methods)
			}
			mu.Lock()
			defer mu.Unlock()
			if stored.Count != tc.wantCount {
				t.Errorf("stored count = %d, want %d", stored.Count, tc.wantCount)
			}
			if stored.Message != tc.wantMessage {
				t.Errorf("stored message = %q, want %q", stored.Message, tc.wantMessage)
			}
			if strings.Join(methods, " | ") != strings.Join(tc.wantVerbs, " | ") {
				t.Errorf("requests = %v, want %v (a node author cannot GET an Event)", methods, tc.wantVerbs)
			}
		})
	}
}

// TestPodEventDetailIsBounded keeps a verbose Macker failure from crowding the Event
// list, while always preserving the pointer to the contract.
func TestPodEventDetailIsBounded(t *testing.T) {
	manager, recorder := newEventTestManager()
	pod := eventTestPod()
	long := strings.Repeat("x", macletEventDetailLimit+500) + " TRAILING DETAIL"
	manager.recordPodEvent(context.Background(), &APIClient{}, pod, "Pending", "MacletMackerLaunchFailed", long)
	events := recorder.recorded()
	if len(events) != 1 {
		t.Fatalf("recorded %d Events, want 1", len(events))
	}
	if len(events[0].Message) > macletEventDetailLimit+len(macletParityReference)+80 {
		t.Errorf("message is %d characters, want it bounded near %d", len(events[0].Message), macletEventDetailLimit)
	}
	if strings.Contains(events[0].Message, "TRAILING DETAIL") {
		t.Error("the tail should be cut, not the part that names the problem")
	}
	if !strings.Contains(events[0].Message, macletParityReference) {
		t.Error("the pointer to the contract must survive truncation")
	}
}
