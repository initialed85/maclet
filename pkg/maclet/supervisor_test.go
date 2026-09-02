package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryableJoinErrorClassifiesControlPlaneFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "service unavailable", err: &HTTPError{Code: http.StatusServiceUnavailable}, want: true},
		{name: "rate limited", err: &HTTPError{Code: http.StatusTooManyRequests}, want: true},
		{name: "unauthorized", err: &HTTPError{Code: http.StatusUnauthorized}, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "pod cidr pending", err: errPodCIDRUnavailable, want: true},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "local", err: errors.New("invalid local configuration"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableJoinError(test.err); got != test.want {
				t.Fatalf("retryableJoinError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestSuperviseJoinRetriesTransientSession(t *testing.T) {
	attempts := 0
	err := superviseJoin(context.Background(), JoinConfig{}, func(context.Context, JoinConfig) error {
		attempts++
		if attempts < 3 {
			return &HTTPError{Code: http.StatusServiceUnavailable}
		}
		return nil
	}, func(int) time.Duration { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("session attempts = %d, want 3", attempts)
	}
}

func TestSuperviseJoinDoesNotRetryNonTransientSession(t *testing.T) {
	attempts := 0
	want := errors.New("invalid local configuration")
	err := superviseJoin(context.Background(), JoinConfig{}, func(context.Context, JoinConfig) error {
		attempts++
		return want
	}, func(int) time.Duration { return 0 })
	if !errors.Is(err, want) {
		t.Fatalf("supervisor error = %v, want %v", err, want)
	}
	if attempts != 1 {
		t.Fatalf("session attempts = %d, want 1", attempts)
	}
}

func TestKubeletTunnelSupervisorRetriesZeroInitialConnections(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1-k3s/apiservers" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode([]string{"https://api.example.test"})
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	var closed atomic.Bool
	connect := func(context.Context, string, string, string, string, string, int) (*kubeletTunnel, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("API server unavailable")
		}
		return &kubeletTunnel{cancel: func() { closed.Store(true) }}, nil
	}
	supervisor := startKubeletTunnelSupervisorWithConnector(context.Background(), client, server.URL, "192.168.137.111", "cert", "key", "ca", defaultKubeletPort, time.Millisecond, connect)
	defer supervisor.Close()
	deadline := time.Now().Add(time.Second)
	for attempts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("tunnel attempts = %d, want retry after initial failure", got)
	}
	supervisor.Close()
	if !closed.Load() {
		t.Fatal("supervisor did not close the connected tunnel")
	}
}

func TestJoinRetryDelayIsCapped(t *testing.T) {
	if got := joinRetryDelay(0); got != time.Second {
		t.Fatalf("initial retry delay = %s, want 1s", got)
	}
	if got := joinRetryDelay(2); got != 4*time.Second {
		t.Fatalf("retry delay = %s, want 4s", got)
	}
	if got := joinRetryDelay(20); got != joinRetryMax {
		t.Fatalf("retry delay = %s, want %s", got, joinRetryMax)
	}
}

func TestReconcileNodeAndLeaseRetainsStateAcrossAPILoss(t *testing.T) {
	node := &Node{ObjectMeta: ObjectMeta{Name: "maclet", ResourceVersion: "7"}}
	nodeUnavailable := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/nodes/maclet":
			if nodeUnavailable {
				nodeUnavailable = false
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(response).Encode(node)
		case request.Method == http.MethodPatch && request.URL.Path == "/api/v1/nodes/maclet/status":
			_ = json.NewEncoder(response).Encode(node)
		case request.Method == http.MethodGet && request.URL.Path == "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/maclet":
			response.WriteHeader(http.StatusNotFound)
		case request.Method == http.MethodPost && request.URL.Path == "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases":
			response.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client, err := newAPIClient(server.URL, nil, "", "", true, "", "")
	if err != nil {
		t.Fatal(err)
	}

	first, err := reconcileNodeAndLease(context.Background(), client, node, "192.168.137.111", "")
	if err == nil || !retryableJoinError(err) {
		t.Fatalf("first reconciliation error = %v, want transient API error", err)
	}
	if first != node {
		t.Fatal("API loss did not retain the existing Node state")
	}

	second, err := reconcileNodeAndLease(context.Background(), client, first, "192.168.137.111", "")
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Name != node.Name {
		t.Fatalf("reconciled Node = %#v", second)
	}
}
