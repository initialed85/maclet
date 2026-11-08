package maclet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const heartbeatNodeName = "maclet"

// fakeControlPlane answers the four calls one status+Lease round makes, with an optional
// delay on the status patch so a stalled round can be observed.
type fakeControlPlane struct {
	delay time.Duration

	mu             sync.Mutex
	statusArrivals []time.Time
	leaseRenewals  []time.Time
}

func (f *fakeControlPlane) record(arrivals *[]time.Time, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*arrivals = append(*arrivals, at)
}

func (f *fakeControlPlane) node(resourceVersion string) Node {
	return Node{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: ObjectMeta{Name: heartbeatNodeName, ResourceVersion: resourceVersion},
		Spec:       NodeSpec{PodCIDR: "10.42.9.0/24"},
		Status: NodeStatus{
			Addresses:  []NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.137.111"}},
			Conditions: []NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "MacletReady"}},
		},
	}
}

func (f *fakeControlPlane) lease() Lease {
	holder := heartbeatNodeName
	stamp := metav1.MicroTime{Time: time.Date(2026, 9, 26, 1, 37, 28, 668847000, time.UTC)}
	duration := int32(defaultLeaseDurationSecs)
	return Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: ObjectMeta{Name: heartbeatNodeName, Namespace: "kube-node-lease", ResourceVersion: "34014913"},
		Spec: LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			AcquireTime:          &stamp,
			RenewTime:            &stamp,
		},
	}
}

func (f *fakeControlPlane) serve(t *testing.T) http.Handler {
	nodePath := "/api/v1/nodes/" + heartbeatNodeName
	statusPath := nodePath + "/status"
	leasePath := "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/" + heartbeatNodeName
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		encode := func(value any) {
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(value); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}
		switch {
		case request.Method == http.MethodGet && request.URL.Path == nodePath:
			encode(f.node("7"))
		case request.Method == http.MethodPatch && request.URL.Path == statusPath:
			f.record(&f.statusArrivals, time.Now())
			if f.delay > 0 {
				time.Sleep(f.delay)
			}
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				t.Errorf("drain status patch: %v", err)
			}
			encode(f.node("8"))
		case request.Method == http.MethodGet && request.URL.Path == leasePath:
			encode(f.lease())
		case request.Method == http.MethodPut && request.URL.Path == leasePath:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read Lease body: %v", err)
				return
			}
			var submitted Lease
			if err := json.Unmarshal(body, &submitted); err != nil {
				t.Errorf("decode Lease body: %v", err)
				return
			}
			if submitted.Spec.RenewTime != nil {
				f.record(&f.leaseRenewals, submitted.Spec.RenewTime.Time)
			}
			encode(submitted)
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func newFakeControlPlane(t *testing.T, delay time.Duration) (*fakeControlPlane, *APIClient) {
	t.Helper()
	fake := &fakeControlPlane{delay: delay}
	server := httptest.NewTLSServer(fake.serve(t))
	t.Cleanup(server.Close)
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return fake, client
}

func (f *fakeControlPlane) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.statusArrivals), len(f.leaseRenewals)
}

func (f *fakeControlPlane) maxStatusGap(t *testing.T) time.Duration {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var worst time.Duration
	for index := 1; index < len(f.statusArrivals); index++ {
		if gap := f.statusArrivals[index].Sub(f.statusArrivals[index-1]); gap > worst {
			worst = gap
		}
	}
	return worst
}

func (f *fakeControlPlane) renewalTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.leaseRenewals...)
}

// A round that the API server keeps waiting on must be cut off by its own deadline: the
// previous shape let one round occupy the whole heartbeat, and a Node that misses the
// cluster's node monitor grace period is NotReady whatever comes after it.
func TestNodeHeartbeatCutsOffSlowRound(t *testing.T) {
	fake, client := newFakeControlPlane(t, 300*time.Millisecond)
	heart := startNodeHeartbeatWith(context.Background(), client, ptrTo(fake.node("7")), "192.168.137.111", "", 10*time.Millisecond, 60*time.Millisecond)
	defer heart.Close()

	started := time.Now()
	heart.renew(context.Background())
	elapsed := time.Since(started)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("round took %s, want it cut off by its 60ms deadline", elapsed)
	}
	heart.mu.Lock()
	lastOK := heart.lastOK
	heart.mu.Unlock()
	if time.Since(lastOK) > time.Second {
		t.Fatalf("a failed round was recorded as a successful renewal (lastOK %s)", lastOK)
	}
	if _, leaseRenewals := fake.counts(); leaseRenewals != 0 {
		t.Fatalf("Lease renewals = %d, want the Lease untouched when the status patch never completed", leaseRenewals)
	}
}

// Cadence is the point of the separate loop: status keeps arriving on schedule even while
// every round fails, because a failing round no longer holds the clock.
func TestNodeHeartbeatKeepsCadenceWhileRoundsFail(t *testing.T) {
	fake, client := newFakeControlPlane(t, 200*time.Millisecond)
	interval := 40 * time.Millisecond
	heart := startNodeHeartbeatWith(context.Background(), client, ptrTo(fake.node("7")), "192.168.137.111", "", interval, 20*time.Millisecond)
	defer heart.Close()
	time.Sleep(600 * time.Millisecond)

	statusRounds, _ := fake.counts()
	if statusRounds < 6 {
		t.Fatalf("status rounds = %d in 600ms at a %s interval, want at least 6", statusRounds, interval)
	}
	if gap := fake.maxStatusGap(t); gap > 150*time.Millisecond {
		t.Fatalf("longest gap between status rounds = %s, want the %s schedule kept even with every round failing", gap, interval)
	}
}

// A successful round has to leave the heartbeat holding the Node the control plane returned,
// with a Lease timestamp the control plane has not seen before.
func TestNodeHeartbeatRecordsRenewedNodeAndLease(t *testing.T) {
	fake, client := newFakeControlPlane(t, 0)
	storedRenew := fake.lease().Spec.RenewTime.Time
	heart := startNodeHeartbeatWith(context.Background(), client, ptrTo(fake.node("7")), "192.168.137.111", "", 30*time.Millisecond, 2*time.Second)

	started := time.Now().UTC().Add(-time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, renewals := fake.counts(); renewals >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	heart.Close()

	statusRounds, leaseRenewals := fake.counts()
	if statusRounds < 2 || leaseRenewals < 2 {
		t.Fatalf("status rounds = %d, Lease renewals = %d, want at least 2 of each", statusRounds, leaseRenewals)
	}
	if node := heart.Node(); node == nil || node.ObjectMeta.ResourceVersion != "8" {
		t.Fatalf("heartbeat Node = %#v, want the object the API server returned", node)
	}
	for index, renew := range fake.renewalTimes() {
		if renew.Equal(storedRenew) {
			t.Fatalf("renewal %d re-sent the stored renewTime %s", index, storedRenew.Format(time.RFC3339Nano))
		}
		if renew.Before(started) {
			t.Fatalf("renewal %d carries %s, older than this test started at %s", index, renew.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano))
		}
	}

	// Close is safe to call again from a deferred cleanup, and it must stop the clock.
	before, _ := fake.counts()
	heart.Close()
	time.Sleep(150 * time.Millisecond)
	after, _ := fake.counts()
	if after != before {
		t.Fatalf("status rounds continued after Close: %d -> %d", before, after)
	}
}

func ptrTo[T any](value T) *T {
	return &value
}
