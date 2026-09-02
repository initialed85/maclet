package maclet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// reconcileNodeAndLease refreshes the authoritative Node before publishing
// status and renewing its Lease. It returns the latest local view even when a
// later status or Lease operation fails, allowing a long-running session to
// retain state and retry on the next heartbeat.
func reconcileNodeAndLease(ctx context.Context, client *APIClient, node *Node, nodeIP, externalIP string) (*Node, error) {
	current := node
	body, err := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(node.ObjectMeta.Name))
	if err != nil {
		return current, fmt.Errorf("refresh Node: %w", err)
	}
	var refreshed Node
	if err := json.Unmarshal(body, &refreshed); err != nil {
		return current, fmt.Errorf("decode refreshed Node: %w", err)
	}
	current = &refreshed
	updated, err := updateNodeStatus(ctx, client, current, nodeIP, externalIP)
	if err != nil {
		return current, err
	}
	if err := ensureLease(ctx, client, current.ObjectMeta.Name); err != nil {
		return updated, err
	}
	return updated, nil
}
