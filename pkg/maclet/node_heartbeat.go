package maclet

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	// defaultNodeRenewInterval is how often Node status and the Node Lease are published.
	// The control plane watches this clock: a k3s server running the flags on this fleet
	// (--node-monitor-period=2s, --node-monitor-grace-period=20s) marks a Node that stops
	// posting NotReady, and its 8-second not-ready tolerations evict what was running there.
	// Liveness therefore gets its own tight cadence instead of riding the slower heartbeat
	// that carries resolver, VXLAN, assigned-Pod and workload reconciles.
	defaultNodeRenewInterval = 5 * time.Second
	// defaultNodeRenewTimeout bounds one status+Lease round. updateNodeStatus retries a
	// conflict up to five times and the HTTP client allows 15 s per call, so an unbounded
	// round can occupy more than a minute and spend the whole grace period before the next
	// attempt. A round that outruns its deadline is cut off and the next one starts on time.
	defaultNodeRenewTimeout = 4 * time.Second
	// defaultNodeRenewWarningPeriod limits how often one failing renewal can be logged.
	// A node whose tunnel is down produces a failure per round, not per incident.
	defaultNodeRenewWarningPeriod = time.Minute
)

// nodeHeartbeat publishes Node status and renews the matching Node Lease on a clock of its
// own, independent of the join heartbeat.
type nodeHeartbeat struct {
	interval   time.Duration
	timeout    time.Duration
	client     *APIClient
	nodeIP     string
	externalIP string

	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	node     *Node
	lastOK   time.Time
	warnedAt time.Time
	failing  bool
}

func startNodeHeartbeat(ctx context.Context, client *APIClient, node *Node, nodeIP, externalIP string) *nodeHeartbeat {
	return startNodeHeartbeatWith(ctx, client, node, nodeIP, externalIP, defaultNodeRenewInterval, defaultNodeRenewTimeout)
}

func startNodeHeartbeatWith(ctx context.Context, client *APIClient, node *Node, nodeIP, externalIP string, interval, timeout time.Duration) *nodeHeartbeat {
	runContext, cancel := context.WithCancel(ctx)
	heartbeat := &nodeHeartbeat{
		interval:   interval,
		timeout:    timeout,
		client:     client,
		nodeIP:     nodeIP,
		externalIP: externalIP,
		node:       node,
		lastOK:     time.Now(),
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go heartbeat.run(runContext)
	return heartbeat
}

// Node returns the newest Node view this heartbeat holds, which is the object the shutdown
// cordon and any later decision must act on.
func (h *nodeHeartbeat) Node() *Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.node
}

// Close stops the renewal loop and waits for the round in flight to finish.
func (h *nodeHeartbeat) Close() {
	if h == nil {
		return
	}
	h.cancel()
	<-h.done
}

func (h *nodeHeartbeat) run(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.renew(ctx)
		}
	}
}

// renew performs one status+Lease round under its own deadline and records whatever Node the
// control plane returned, even when the round failed, so a later round retries from the
// authoritative object rather than a stale one.
func (h *nodeHeartbeat) renew(ctx context.Context) {
	roundContext, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	started := time.Now()
	h.mu.Lock()
	node := h.node
	h.mu.Unlock()

	updated, err := reconcileNodeAndLease(roundContext, h.client, node, h.nodeIP, h.externalIP)
	elapsed := time.Since(started)

	h.mu.Lock()
	if updated != nil {
		h.node = updated
	}
	now := time.Now()
	gap := now.Sub(h.lastOK)
	if err == nil {
		h.lastOK = now
		if h.failing {
			h.failing = false
			log.Printf("Node %q status and Lease renewed after %s without a successful round", nodeNameFor(h.node, node), gap.Round(time.Second))
		}
	} else {
		h.failing = true
		if errors.Is(err, context.Canceled) {
			// The session is ending; a round cut off by its own parent context is not an outage.
			h.mu.Unlock()
			return
		}
		if h.warnedAt.IsZero() || now.Sub(h.warnedAt) >= defaultNodeRenewWarningPeriod {
			h.warnedAt = now
			log.Printf("warning: Node %q status and Lease renewal failed after %s (%s without a successful round); retaining local runtime: %v", nodeNameFor(h.node, node), elapsed.Round(100*time.Microsecond), gap.Round(time.Second), err)
		}
	}
	h.mu.Unlock()

	if err == nil && elapsed > h.interval {
		log.Printf("warning: Node %q status and Lease round took %s, longer than its %s schedule", nodeNameFor(h.node, node), elapsed.Round(100*time.Microsecond), h.interval)
	}
}

func nodeNameFor(nodes ...*Node) string {
	for _, node := range nodes {
		if node != nil && node.ObjectMeta.Name != "" {
			return node.ObjectMeta.Name
		}
	}
	return ""
}
