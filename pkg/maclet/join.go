package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func runJoin(cfg JoinConfig) error {
	if err := preparePrivileges(&cfg); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	state, client, err := bootstrap(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.NodeName == "" {
		cfg.NodeName = state.NodeName
	}
	if cfg.NodeIP == "" {
		cfg.NodeIP = state.NodeIP
	}
	if cfg.ExternalIP == "" {
		cfg.ExternalIP = state.ExternalIP
		if cfg.ExternalIP == "" {
			cfg.ExternalIP = vxlanPublicIP(cfg, state)
		}
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = defaultDrainTimeout
	}
	if cfg.PeerKubeconfig == "" {
		cfg.PeerKubeconfig = state.PeerKubeconfig
	}
	if cfg.PeerContext == "" {
		cfg.PeerContext = state.PeerContext
	}
	node, err := ensureNode(ctx, client, state.NodeName, state.NodeIP)
	if err != nil {
		return err
	}
	// Only remove a cordon that maclet itself installed during a previous
	// graceful shutdown. Preserve an operator's independent cordon.
	if node.ObjectMeta.Annotations[shutdownCordonAnnotation] == "true" {
		node, err = setNodeShutdownCordon(ctx, client, node, false)
		if err != nil {
			return err
		}
	}
	log.Printf("Node %s registered with labels kubernetes.io/os=darwin kubernetes.io/arch=arm64 and taint %s=%s:NoSchedule", state.NodeName, managedTaintKey, managedTaintValue)
	node, err = waitForPodCIDR(ctx, client, node, 60*time.Second)
	if err != nil {
		return fmt.Errorf("wait for PodCIDR: %w", err)
	}
	if node.Spec.PodCIDR != "" {
		log.Printf("Kubernetes assigned PodCIDR %s", node.Spec.PodCIDR)
	} else {
		log.Printf("Kubernetes has not assigned a PodCIDR yet; continuing without starting VXLAN")
	}
	var peerClient *APIClient
	var cleanupClient *APIClient
	var peers []FlannelPeer
	var gatewayMAC string
	if cfg.VXLANBinary != "" && cfg.VXLANGatewayMAC == "" {
		peerClient, err = peerAPIClient(cfg, state)
		if err != nil {
			return err
		}
		peers, err = discoverFlannelPeers(ctx, peerClient, state.NodeName)
		if err != nil {
			return err
		}
		for _, peer := range peers {
			if peer.PublicIP == cfg.VXLANRemote {
				gatewayMAC = peer.VtepMAC
				break
			}
		}
		if gatewayMAC == "" {
			return fmt.Errorf("no Flannel peer matches VXLAN remote %q; use --vxlan-gateway-mac to provide that node's flannel.1 MAC", cfg.VXLANRemote)
		}
	}
	vxlan, err := startVXLAN(ctx, cfg, node, peers)
	if err != nil {
		return err
	}
	var darwinNetwork *DarwinNetworkHandle
	var workloads *workloadManager
	var dnsResolver *clusterDNSResolver
	if vxlan != nil {
		if gatewayMAC == "" {
			gatewayMAC, err = remoteVtepMAC(ctx, client, cfg.VXLANRemote, cfg.VXLANGatewayMAC)
			if err != nil {
				vxlan.cleanup()
				return err
			}
		}
		darwinNetwork, err = setupDarwinNetwork(cfg, vxlan, peers, gatewayMAC)
		if err != nil {
			vxlan.cleanup()
			return err
		}
		node, err = configureFlannel(ctx, client, node, vxlanPublicIP(cfg, state), vxlan.BridgeMAC)
		if err != nil {
			if cleanupErr := darwinNetwork.cleanup(); cleanupErr != nil {
				log.Printf("warning: clean Darwin routes after Flannel setup failure: %v", cleanupErr)
			}
			vxlan.cleanup()
			return err
		}
		log.Printf("published Flannel VXLAN metadata for %s: publicIP=%s vtepMAC=%s gatewayMAC=%s", state.NodeName, vxlanPublicIP(cfg, state), vxlan.BridgeMAC, gatewayMAC)
		workloads = newWorkloadManagerWithState(darwinNetwork, cfg.MackerBinary, state.NodeIP, cfg.StateDir)
		workloads.debug = cfg.Debug
		if err := workloads.loadJournal(); err != nil {
			darwinNetwork.cleanup()
			vxlan.cleanup()
			return fmt.Errorf("load native workload journal: %w", err)
		}
		defer func() {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if workloads != nil {
				if err := workloads.cleanup(); err != nil {
					log.Printf("warning: clean native workloads: %v", err)
				}
			}
			if err := clearFlannel(cleanupContext, client, state.NodeName); err != nil {
				log.Printf("warning: clear Flannel annotations: %v", err)
			}
			if err := darwinNetwork.cleanup(); err != nil {
				log.Printf("warning: clean Darwin routes: %v", err)
			}
			vxlan.cleanup()
		}()
		if !cfg.Once && cfg.DNSResolver {
			dnsResolver = newClusterDNSResolver(cfg.useSudo)
			resolverErr := dnsResolver.reconcile(ctx, client)
			if resolverErr != nil && cleanupClient != nil {
				resolverErr = dnsResolver.reconcile(ctx, cleanupClient)
			}
			if resolverErr != nil {
				log.Printf("warning: configure cluster DNS resolver: %v", resolverErr)
			} else {
				log.Printf("configured macOS resolver %s for cluster DNS via %s", dnsResolver.path, strings.Join(dnsResolver.nameservers, ", "))
			}
			defer func() {
				cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := dnsResolver.cleanup(cleanupContext); err != nil {
					log.Printf("warning: clean cluster DNS resolver: %v", err)
				}
			}()
		}
	}
	var cleanupStaleNativePods func(context.Context, []Pod)
	if !cfg.Once && workloads != nil {
		// The system:node identity is intentionally not allowed to delete Pods.
		// Reuse only an explicitly configured peer identity for force-deleting
		// native Pods whose graceful deletion has outlived the kubelet grace period;
		// the K3s controller certificate is read-only for this purpose.
		if configured, found, peerErr := configuredPeerAPIClient(cfg, state); peerErr != nil {
			log.Printf("warning: stale native Pod cleanup unavailable: %v", peerErr)
		} else if found {
			cleanupClient = configured
		}
		var cleanupWarningAt time.Time
		cleanupStaleNativePods = func(cleanupContext context.Context, pods []Pod) {
			if cleanupClient == nil {
				return
			}
			removed, cleanupErr := cleanupPodList(cleanupContext, cleanupClient, pods, state.NodeName, defaultNativePodCleanupStaleAfter, time.Now())
			if removed > 0 {
				log.Printf("removed %d stale native Pod(s) assigned to %s", removed, state.NodeName)
			}
			if cleanupErr != nil && (cleanupWarningAt.IsZero() || time.Since(cleanupWarningAt) >= time.Minute) {
				cleanupWarningAt = time.Now()
				log.Printf("warning: stale native Pod cleanup: %v", cleanupErr)
			}
		}
	}
	if !cfg.Once && workloads != nil {
		kubelet, kubeletErr := startKubeletServer(ctx, state.NodeIP, workloads, state.ServingCert, state.ServingKey, state.ClientCA, defaultKubeletPort)
		if kubeletErr != nil {
			return kubeletErr
		}
		defer func() {
			if err := kubelet.Close(); err != nil {
				log.Printf("warning: close kubelet HTTPS server: %v", err)
			}
		}()
		tunnelServers := discoverKubeletTunnelServers(ctx, client, state.Server)
		connectedTunnels := 0
		for _, tunnelServer := range tunnelServers {
			tunnel, tunnelErr := startKubeletTunnel(ctx, tunnelServer, state.NodeIP, state.ClientCert, state.ClientKey, state.CAFile, defaultKubeletPort)
			if tunnelErr != nil {
				log.Printf("warning: kubelet tunnel to %s unavailable: %v", tunnelServer, tunnelErr)
				continue
			}
			connectedTunnels++
			defer tunnel.Close()
		}
		if connectedTunnels == 0 {
			return errors.New("no Kubernetes API server accepted a kubelet remotedialer tunnel")
		}
	}
	node, err = updateNodeStatus(ctx, client, node, state.NodeIP, cfg.ExternalIP)
	if err != nil {
		return err
	}
	if err := ensureLease(ctx, client, state.NodeName); err != nil {
		return err
	}
	if !cfg.Once && workloads != nil {
		if pods, listErr := listAssignedPods(ctx, client, state.NodeName); listErr != nil {
			log.Printf("warning: list assigned workloads: %v", listErr)
		} else {
			if reconcileErr := workloads.reconcile(ctx, client, pods); reconcileErr != nil {
				log.Printf("warning: reconcile native workloads: %v", reconcileErr)
			}
			if cleanupStaleNativePods != nil {
				cleanupStaleNativePods(ctx, pods)
			}
		}
	}

	if cfg.Once {
		return nil
	}
	ticker := time.NewTicker(defaultHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			drainContext, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
			if node != nil {
				if drainedNode, drainErr := setNodeShutdownCordon(drainContext, client, node, true); drainErr != nil {
					log.Printf("warning: cordon Node before shutdown: %v", drainErr)
				} else {
					node = drainedNode
				}
			}
			if workloads != nil {
				if cleanupErr := workloads.cleanup(); cleanupErr != nil {
					log.Printf("warning: drain native workloads: %v", cleanupErr)
				}
			}
			cancel()
			return nil
		case <-ticker.C:
			if dnsResolver != nil {
				resolverErr := dnsResolver.reconcile(ctx, client)
				if resolverErr != nil && cleanupClient != nil {
					resolverErr = dnsResolver.reconcile(ctx, cleanupClient)
				}
				if resolverErr != nil {
					log.Printf("warning: refresh cluster DNS resolver: %v", resolverErr)
				}
			}
			if darwinNetwork != nil && peerClient != nil {
				gatewayMAC, peerErr := remoteVtepMAC(ctx, peerClient, cfg.VXLANRemote, "")
				if peerErr != nil {
					log.Printf("warning: refresh Flannel gateway MAC: %v", peerErr)
				} else if updateErr := darwinNetwork.setGatewayMAC(gatewayMAC); updateErr != nil {
					log.Printf("warning: update Flannel gateway MAC: %v", updateErr)
				}
			}
			body, err := client.Get(ctx, "/api/v1/nodes/"+url.PathEscape(state.NodeName))
			if err != nil {
				return fmt.Errorf("refresh Node: %w", err)
			}
			if err := json.Unmarshal(body, &node); err != nil {
				return err
			}
			node, err = updateNodeStatus(ctx, client, node, state.NodeIP, cfg.ExternalIP)
			if err != nil {
				return err
			}
			if err := ensureLease(ctx, client, state.NodeName); err != nil {
				return err
			}
			if workloads != nil {
				if pods, listErr := listAssignedPods(ctx, client, state.NodeName); listErr != nil {
					log.Printf("warning: list assigned workloads: %v", listErr)
				} else {
					if reconcileErr := workloads.reconcile(ctx, client, pods); reconcileErr != nil {
						log.Printf("warning: reconcile native workloads: %v", reconcileErr)
					}
					if cleanupStaleNativePods != nil {
						cleanupStaleNativePods(ctx, pods)
					}
				}
			}
		}
	}
}
