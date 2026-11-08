package maclet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const probeLeaseNodeName = "icarus-2.local"

// staleLease is the shape kube-node-lease held on the fleet for six weeks: a Lease whose
// holder and duration are correct, and whose timestamps have not moved since creation.
func staleLease() Lease {
	stamp := metav1.MicroTime{Time: time.Date(2026, 9, 26, 1, 37, 28, 668847000, time.UTC)}
	holder := probeLeaseNodeName
	duration := int32(defaultLeaseDurationSecs)
	return Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: ObjectMeta{Name: probeLeaseNodeName, Namespace: "kube-node-lease", ResourceVersion: "34014913"},
		Spec: LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &duration,
			AcquireTime:          &stamp,
			RenewTime:            &stamp,
		},
	}
}

func TestEnsureLeaseRenewsStoredLease(t *testing.T) {
	stored := staleLease()
	storedRenew := stored.Spec.RenewTime.Time
	var renewTimes []time.Time
	var acquireTimes []time.Time
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet:
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&stored); err != nil {
				t.Errorf("encode stored Lease: %v", err)
			}
		case request.Method == http.MethodPut:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read PUT body: %v", err)
				return
			}
			var submitted Lease
			if err := json.Unmarshal(body, &submitted); err != nil {
				t.Errorf("decode PUT body: %v", err)
				return
			}
			if submitted.Spec.HolderIdentity == nil || *submitted.Spec.HolderIdentity != probeLeaseNodeName {
				t.Errorf("holder = %#v, want the Node name", submitted.Spec.HolderIdentity)
			}
			if submitted.Spec.LeaseDurationSeconds == nil || *submitted.Spec.LeaseDurationSeconds != int32(defaultLeaseDurationSecs) {
				t.Errorf("leaseDurationSeconds = %#v, want %d", submitted.Spec.LeaseDurationSeconds, defaultLeaseDurationSecs)
			}
			if submitted.Spec.RenewTime == nil {
				t.Errorf("PUT body has no renewTime")
				return
			}
			renewTimes = append(renewTimes, submitted.Spec.RenewTime.Time)
			if submitted.Spec.AcquireTime != nil {
				acquireTimes = append(acquireTimes, submitted.Spec.AcquireTime.Time)
			}
			accepted := stored
			accepted.ResourceVersion = "34014914"
			accepted.Spec = submitted.Spec
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&accepted); err != nil {
				t.Errorf("encode accepted Lease: %v", err)
			}
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Second)
	for round := 0; round < 2; round++ {
		if err := ensureLease(context.Background(), client, probeLeaseNodeName); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if len(renewTimes) != 2 {
		t.Fatalf("renewals sent = %d, want 2", len(renewTimes))
	}
	for round, renew := range renewTimes {
		if renew.Equal(storedRenew) {
			t.Fatalf("round %d sent the stored renewTime %s back instead of renewing it", round, renew.Format(time.RFC3339Nano))
		}
		if renew.Before(started) {
			t.Fatalf("round %d renewTime %s is older than this test started at %s", round, renew.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano))
		}
	}
	if len(acquireTimes) != 2 {
		t.Fatalf("acquireTimes sent = %d, want 2", len(acquireTimes))
	}
	for round, acquire := range acquireTimes {
		if !acquire.Equal(stored.Spec.AcquireTime.Time) {
			t.Fatalf("round %d rewrote acquireTime to %s, want the stored %s", round, acquire.Format(time.RFC3339Nano), stored.Spec.AcquireTime.Time.Format(time.RFC3339Nano))
		}
	}
}

func TestEnsureLeaseCreatesMissingLease(t *testing.T) {
	var created Lease
	var createdPath string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet:
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		case request.Method == http.MethodPost:
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read POST body: %v", err)
				return
			}
			if err := json.Unmarshal(body, &created); err != nil {
				t.Errorf("decode POST body: %v", err)
				return
			}
			createdPath = request.URL.Path
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&created); err != nil {
				t.Errorf("encode created Lease: %v", err)
			}
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Second)
	if err := ensureLease(context.Background(), client, probeLeaseNodeName); err != nil {
		t.Fatal(err)
	}
	if createdPath != "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases" {
		t.Fatalf("create path = %q", createdPath)
	}
	if created.Spec.AcquireTime == nil || created.Spec.RenewTime == nil {
		t.Fatalf("created Lease has no acquireTime or renewTime: %#v", created.Spec)
	}
	if created.Spec.AcquireTime.Time.Before(started) || created.Spec.RenewTime.Time.Before(started) {
		t.Fatalf("created Lease timestamps %s/%s are older than this test started at %s", created.Spec.AcquireTime.Time, created.Spec.RenewTime.Time, started)
	}
	if created.ObjectMeta.Namespace != "kube-node-lease" || created.ObjectMeta.Name != probeLeaseNodeName {
		t.Fatalf("created Lease identity = %s/%s", created.ObjectMeta.Namespace, created.ObjectMeta.Name)
	}
}
