package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
)

type flannelBackendData struct {
	VNI     int    `json:"VNI"`
	VtepMAC string `json:"VtepMAC"`
}

func flannelAnnotations(publicIP, bridgeMAC string) (map[string]string, error) {
	backendData, err := json.Marshal(flannelBackendData{VNI: 1, VtepMAC: bridgeMAC})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		flannelBackendTypeAnnotation:   "vxlan",
		flannelBackendDataAnnotation:   string(backendData),
		flannelPublicIPAnnotation:      publicIP,
		flannelSubnetManagerAnnotation: "true",
	}, nil
}

func configureFlannel(ctx context.Context, client *APIClient, node *Node, publicIP, bridgeMAC string) (*Node, error) {
	desired, err := flannelAnnotations(publicIP, bridgeMAC)
	if err != nil {
		return nil, err
	}
	current := node
	for attempt := 0; attempt < 5; attempt++ {
		annotations := make(map[string]string, len(current.ObjectMeta.Annotations)+len(desired))
		for key, value := range current.ObjectMeta.Annotations {
			annotations[key] = value
		}
		changed := false
		for key, value := range desired {
			if annotations[key] != value {
				changed = true
			}
			annotations[key] = value
		}
		if !changed {
			return current, nil
		}
		body, patchErr := client.PatchJSON(ctx, "/api/v1/nodes/"+url.PathEscape(current.ObjectMeta.Name), map[string]any{
			"metadata": map[string]any{"annotations": annotations},
		})
		if patchErr == nil {
			var updated Node
			if err := json.Unmarshal(body, &updated); err != nil {
				return nil, fmt.Errorf("decode Flannel Node patch: %w", err)
			}
			return &updated, nil
		}
		var conflict *HTTPError
		if !errors.As(patchErr, &conflict) || conflict.Code != http.StatusConflict || attempt == 4 {
			return nil, fmt.Errorf("configure Flannel annotations: %w", patchErr)
		}
		latest, getErr := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(current.ObjectMeta.Name))
		if getErr != nil {
			return nil, fmt.Errorf("refresh Node after Flannel annotation conflict: %w", getErr)
		}
		var refreshed Node
		if getErr := json.Unmarshal(latest, &refreshed); getErr != nil {
			return nil, fmt.Errorf("decode Node after Flannel annotation conflict: %w", getErr)
		}
		current = &refreshed
	}
	return nil, errors.New("Flannel annotation retry limit exceeded")
}

func clearFlannel(ctx context.Context, client *APIClient, nodeName string) error {
	path := "/api/v1/nodes/" + url.PathEscape(nodeName)
	body, err := client.Get(ctx, path)
	if err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return err
	}
	var node Node
	if err := json.Unmarshal(body, &node); err != nil {
		return err
	}
	remove := map[string]any{}
	for _, key := range []string{
		flannelBackendTypeAnnotation,
		flannelBackendDataAnnotation,
		flannelPublicIPAnnotation,
		flannelSubnetManagerAnnotation,
	} {
		if _, ok := node.ObjectMeta.Annotations[key]; ok {
			remove[key] = nil
		}
	}
	if len(remove) == 0 {
		return nil
	}
	if _, err := client.PatchJSON(ctx, path, map[string]any{"metadata": map[string]any{"annotations": remove}}); err != nil {
		return fmt.Errorf("clear Flannel annotations: %w", err)
	}
	return nil
}

func discoverFlannelPeers(ctx context.Context, client *APIClient, localNodeName string) ([]FlannelPeer, error) {
	body, err := client.Get(ctx, "/api/v1/nodes")
	if err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusForbidden {
			return nil, errors.New("the peer client cannot read Nodes; provide a peer kubeconfig with Node list access or use --vxlan-gateway-mac for single-peer mode")
		}
		return nil, fmt.Errorf("list Nodes to discover Flannel peers: %w", err)
	}
	var nodes NodeList
	if err := json.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("decode Node list for Flannel peers: %w", err)
	}
	peers := make([]FlannelPeer, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.ObjectMeta.Name == localNodeName {
			continue
		}
		annotations := node.ObjectMeta.Annotations
		if annotations[flannelBackendTypeAnnotation] != "vxlan" {
			continue
		}
		cidr := node.Spec.PodCIDR
		if cidr == "" && len(node.Spec.PodCIDRs) > 0 {
			cidr = node.Spec.PodCIDRs[0]
		}
		if cidr == "" || annotations[flannelPublicIPAnnotation] == "" || annotations[flannelBackendDataAnnotation] == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(cidr); err != nil || network.IP.To4() == nil {
			continue
		}
		var backend flannelBackendData
		if err := json.Unmarshal([]byte(annotations[flannelBackendDataAnnotation]), &backend); err != nil {
			return nil, fmt.Errorf("decode Flannel backend data for node %s: %w", node.ObjectMeta.Name, err)
		}
		if backend.VNI != 1 {
			continue
		}
		mac, err := net.ParseMAC(backend.VtepMAC)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("node %s has invalid Flannel VtepMAC %q", node.ObjectMeta.Name, backend.VtepMAC)
		}
		if net.ParseIP(annotations[flannelPublicIPAnnotation]) == nil {
			return nil, fmt.Errorf("node %s has invalid Flannel public IP %q", node.ObjectMeta.Name, annotations[flannelPublicIPAnnotation])
		}
		peers = append(peers, FlannelPeer{
			NodeName: node.ObjectMeta.Name,
			PodCIDR:  cidr,
			PublicIP: annotations[flannelPublicIPAnnotation],
			VtepMAC:  mac.String(),
		})
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].PodCIDR < peers[j].PodCIDR
	})
	return peers, nil
}

func remoteVtepMAC(ctx context.Context, client *APIClient, remoteIP, override string) (string, error) {
	if override != "" {
		mac, err := net.ParseMAC(override)
		if err != nil || len(mac) != 6 {
			return "", fmt.Errorf("invalid --vxlan-gateway-mac %q", override)
		}
		return mac.String(), nil
	}
	peers, err := discoverFlannelPeers(ctx, client, "")
	if err != nil {
		return "", err
	}
	for _, peer := range peers {
		if peer.PublicIP == remoteIP {
			return peer.VtepMAC, nil
		}
	}
	return "", fmt.Errorf("no Flannel node annotation matches VXLAN remote %q; use --vxlan-gateway-mac to provide that node's flannel.1 MAC", remoteIP)
}
