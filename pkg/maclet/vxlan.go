package maclet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

var errPodCIDRUnavailable = errors.New("PodCIDR is not assigned")

func startVXLAN(ctx context.Context, cfg JoinConfig, node *Node, peers []FlannelPeer) (*VXLANHandle, error) {
	if cfg.VXLANBinary == "" {
		return nil, nil
	}
	if cfg.VXLANRemote == "" {
		return nil, errors.New("--vxlan-remote is required when --vxlan-binary is set")
	}
	cidr := node.Spec.PodCIDR
	if cidr == "" && len(node.Spec.PodCIDRs) > 0 {
		cidr = node.Spec.PodCIDRs[0]
	}
	if cidr == "" {
		return nil, fmt.Errorf("%w: cannot start VXLAN until Kubernetes assigns this node a PodCIDR", errPodCIDRUnavailable)
	}
	bridgeCIDR, err := bridgeAddressForCIDR(cidr)
	if err != nil {
		return nil, err
	}
	local := cfg.VXLANLocal
	if local == "" {
		local = cfg.NodeIP
	}
	if local == "" {
		return nil, errors.New("cannot start VXLAN without a local underlay address")
	}
	arguments := []string{
		"--vni", "1",
		"--local", local,
		// Keep the selected peer as a fallback for service traffic and for
		// older darwin-vxlan binaries. New binaries use the peer MAC mappings
		// below for destination-specific PodCIDR traffic.
		"--remote", cfg.VXLANRemote,
		"--port", fmt.Sprint(cfg.VXLANPort),
		"--mtu", fmt.Sprint(cfg.VXLANMTU),
		"--bridge-ipv4", bridgeCIDR,
	}
	for _, peer := range peers {
		// maclet installs per-PodCIDR synthetic gateways with each peer's
		// VtepMAC. darwin-vxlan uses the inner destination IP to select this
		// peer's underlay endpoint while preserving the Ethernet frame.
		arguments = append(arguments, "--peer", peer.PodCIDR+"="+peer.PublicIP)
	}
	// ClusterIP traffic must use the selected Linux node as its service
	// gateway; kube-proxy on that node can then select the actual backend.
	arguments = append(arguments, "--peer", cfg.ServiceCIDR+"="+cfg.VXLANRemote)
	if recovered, err := recoverStaleVXLAN(cfg, bridgeCIDR, local); err != nil {
		return nil, fmt.Errorf("recover stale VXLAN state: %w", err)
	} else if recovered {
		log.Printf("recovered stale VXLAN state for bridge address %s", bridgeCIDR)
	}
	var command *exec.Cmd
	for attempt := 0; ; attempt++ {
		if cfg.useSudo {
			command = exec.Command("sudo", append([]string{"-n", cfg.VXLANBinary}, arguments...)...)
		} else {
			command = exec.Command(cfg.VXLANBinary, arguments...)
		}
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err == nil {
			break
		} else if attempt == 0 {
			// A process can appear between stale-state inspection and
			// command.Start. Retry once after applying the same narrow
			// ownership check rather than killing arbitrary VXLAN users.
			if recovered, recoveryErr := recoverStaleVXLAN(cfg, bridgeCIDR, local); recoveryErr != nil {
				return nil, fmt.Errorf("start darwin-vxlan: %w (stale-state recovery: %v)", err, recoveryErr)
			} else if recovered {
				log.Printf("recovered stale VXLAN state after startup failure")
				continue
			}
			return nil, fmt.Errorf("start darwin-vxlan: %w", err)
		} else {
			return nil, fmt.Errorf("start darwin-vxlan after stale-state recovery: %w", err)
		}
	}
	log.Printf("started VXLAN child pid=%d podCIDR=%s bridgeCIDR=%s", command.Process.Pid, cidr, bridgeCIDR)
	wait := make(chan error, 1)
	processDone := make(chan struct{})
	go func() {
		wait <- command.Wait()
		close(processDone)
	}()
	cleanup := func() {
		if command.Process == nil {
			return
		}
		signalProcessTree(command.Process.Pid, cfg.useSudo, syscall.SIGINT)
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			signalProcessTree(command.Process.Pid, cfg.useSudo, syscall.SIGKILL)
			select {
			case <-wait:
			case <-time.After(time.Second):
			}
		}
	}
	bridgeName, bridgeMAC, err := waitForBridge(ctx, bridgeCIDR, 30*time.Second, processDone)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("discover darwin-vxlan bridge: %w", err)
	}
	select {
	case <-processDone:
		cleanup()
		return nil, errors.New("darwin-vxlan exited after creating its bridge")
	default:
	}
	log.Printf("VXLAN bridge discovered name=%s mac=%s address=%s", bridgeName, bridgeMAC, bridgeCIDR)
	return &VXLANHandle{BridgeCIDR: bridgeCIDR, BridgeName: bridgeName, BridgeMAC: bridgeMAC, cleanup: cleanup}, nil
}
