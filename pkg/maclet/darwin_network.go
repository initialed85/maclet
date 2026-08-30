package maclet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

func gatewayAddressForCIDR(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return "", fmt.Errorf("PodCIDR %q does not have a second usable IPv4 address", cidr)
	}
	value := binary.BigEndian.Uint32(ip4) + 2
	gateway := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(gateway, value)
	return gateway.String(), nil
}

func routeSpec(cidr string) (DarwinRoute, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return DarwinRoute{}, fmt.Errorf("parse network CIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return DarwinRoute{}, fmt.Errorf("network CIDR %q is not IPv4", cidr)
	}
	mask := network.Mask
	maskIP := net.IPv4(mask[0], mask[1], mask[2], mask[3]).String()
	return DarwinRoute{Network: ip4.String(), Netmask: maskIP}, nil
}

func workloadIPForOffset(cidr string, offset uint32) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	hostBits := bits - prefix
	if hostBits < 2 || hostBits > 16 {
		return "", fmt.Errorf("PodCIDR %q is outside the supported workload address range", cidr)
	}
	broadcastOffset := uint32(1<<hostBits) - 1
	// Offset 0 is the network address, offsets 1 and 2 are reserved for the
	// maclet bridge and synthetic Flannel gateway, and the final offset is the
	// broadcast address.
	if offset < 3 || offset >= broadcastOffset {
		return "", fmt.Errorf("workload address offset %d is unavailable in PodCIDR %q", offset, cidr)
	}
	value := binary.BigEndian.Uint32(ip4) + offset
	workloadIP := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(workloadIP, value)
	return workloadIP.String(), nil
}

func firstAvailableWorkloadIP(cidr string, used map[string]bool) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	hostBits := bits - prefix
	if hostBits < 2 || hostBits > 16 {
		return "", fmt.Errorf("PodCIDR %q is outside the supported workload address range", cidr)
	}
	broadcastOffset := uint32(1<<hostBits) - 1
	for offset := uint32(3); offset < broadcastOffset; offset++ {
		ip, err := workloadIPForOffset(cidr, offset)
		if err != nil {
			return "", err
		}
		if !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("PodCIDR %q has no available workload addresses", cidr)
}

func peerGatewayAddressForCIDR(cidr string, index int) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	ip4 := network.IP.To4()
	if ip4 == nil {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return "", fmt.Errorf("PodCIDR %q does not have enough gateway addresses", cidr)
	}
	hostBits := bits - prefix
	broadcastOffset := uint32(1<<hostBits) - 1
	var offset uint32
	if index == 0 {
		offset = 2
	} else {
		if uint32(index) >= broadcastOffset-2 {
			return "", fmt.Errorf("PodCIDR %q has no address for peer gateway %d", cidr, index)
		}
		offset = broadcastOffset - uint32(index)
	}
	value := binary.BigEndian.Uint32(ip4) + offset
	gateway := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(gateway, value)
	return gateway.String(), nil
}

func setupDarwinNetwork(cfg JoinConfig, vxlan *VXLANHandle, peers []FlannelPeer, gatewayMAC string) (*DarwinNetworkHandle, error) {
	gateway, err := peerGatewayAddressForCIDR(vxlan.BridgeCIDR, 0)
	if err != nil {
		return nil, err
	}
	mac, err := net.ParseMAC(gatewayMAC)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("parse Flannel gateway MAC %q", gatewayMAC)
	}
	orderedPeers := make([]FlannelPeer, 0, len(peers))
	for _, peer := range peers {
		if peer.PublicIP == cfg.VXLANRemote {
			orderedPeers = append(orderedPeers, peer)
		}
	}
	for _, peer := range peers {
		if peer.PublicIP != cfg.VXLANRemote {
			orderedPeers = append(orderedPeers, peer)
		}
	}
	if len(orderedPeers) > 0 && orderedPeers[0].PublicIP != cfg.VXLANRemote {
		return nil, fmt.Errorf("no discovered Flannel peer matches VXLAN remote %q", cfg.VXLANRemote)
	}
	handle := &DarwinNetworkHandle{
		Interface:  vxlan.BridgeName,
		PodCIDR:    vxlan.BridgeCIDR,
		Gateway:    gateway,
		GatewayMAC: mac.String(),
		useSudo:    cfg.useSudo,
	}
	addGateway := func(peer FlannelPeer, peerGateway, peerMAC string) error {
		for _, existing := range handle.PeerGateways {
			if existing.Gateway == peerGateway || existing.PodCIDR == peer.PodCIDR {
				return fmt.Errorf("duplicate Darwin peer gateway %s or PodCIDR %s", peerGateway, peer.PodCIDR)
			}
		}
		handle.PeerGateways = append(handle.PeerGateways, DarwinPeerGateway{
			PodCIDR: peer.PodCIDR, Gateway: peerGateway, MAC: peerMAC, PublicIP: peer.PublicIP,
		})
		handle.ARPs = append(handle.ARPs, DarwinARPEntry{IP: peerGateway, MAC: peerMAC})
		return nil
	}
	if len(orderedPeers) == 0 {
		if err := addGateway(FlannelPeer{PublicIP: cfg.VXLANRemote}, gateway, mac.String()); err != nil {
			return nil, err
		}
	} else {
		for index, peer := range orderedPeers {
			peerMAC, parseErr := net.ParseMAC(peer.VtepMAC)
			if parseErr != nil || len(peerMAC) != 6 {
				return nil, fmt.Errorf("parse Flannel VtepMAC %q for node %s", peer.VtepMAC, peer.NodeName)
			}
			peerGateway, gatewayErr := peerGatewayAddressForCIDR(vxlan.BridgeCIDR, index)
			if gatewayErr != nil {
				return nil, gatewayErr
			}
			if err := addGateway(peer, peerGateway, peerMAC.String()); err != nil {
				return nil, err
			}
		}
	}
	routes := make([]DarwinRoute, 0, 2+len(orderedPeers))
	for _, cidr := range []string{cfg.ClusterCIDR, cfg.ServiceCIDR} {
		route, err := routeSpec(cidr)
		if err != nil {
			return nil, err
		}
		route.Gateway = gateway
		routes = append(routes, route)
	}
	for _, peer := range orderedPeers {
		route, err := routeSpec(peer.PodCIDR)
		if err != nil {
			return nil, err
		}
		for _, peerGateway := range handle.PeerGateways {
			if peerGateway.PodCIDR == peer.PodCIDR {
				route.Gateway = peerGateway.Gateway
				break
			}
		}
		routes = append(routes, route)
	}
	for _, arp := range handle.ARPs {
		command := privilegedCommand(cfg.useSudo, "arp", "-S", arp.IP, arp.MAC, "ifscope", vxlan.BridgeName)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErr := handle.cleanup()
			if cleanupErr != nil {
				return nil, fmt.Errorf("install Darwin ARP gateway %s on %s: %w (cleanup: %v; output: %s)", arp.IP, vxlan.BridgeName, err, cleanupErr, strings.TrimSpace(string(output)))
			}
			return nil, fmt.Errorf("install Darwin ARP gateway %s on %s: %w (%s)", arp.IP, vxlan.BridgeName, err, strings.TrimSpace(string(output)))
		}
	}
	for _, route := range routes {
		command := privilegedCommand(cfg.useSudo, "route", "-n", "add", "-net", route.Network, "-netmask", route.Netmask, route.Gateway)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErr := handle.cleanup()
			if cleanupErr != nil {
				return nil, fmt.Errorf("add Darwin route %s/%s via %s: %w (cleanup: %v; output: %s)", route.Network, route.Netmask, route.Gateway, err, cleanupErr, strings.TrimSpace(string(output)))
			}
			return nil, fmt.Errorf("add Darwin route %s/%s via %s: %w (%s)", route.Network, route.Netmask, route.Gateway, err, strings.TrimSpace(string(output)))
		}
		handle.Routes = append(handle.Routes, route)
	}
	return handle, nil
}

func (h *DarwinNetworkHandle) addWorkloadIP(ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return fmt.Errorf("workload address %q is not an IPv4 address", ip)
	}
	parsed = parsed.To4()
	_, network, err := net.ParseCIDR(h.PodCIDR)
	if err != nil {
		return fmt.Errorf("parse PodCIDR %q: %w", h.PodCIDR, err)
	}
	if !network.Contains(parsed) {
		return fmt.Errorf("workload address %s is outside PodCIDR %s", ip, h.PodCIDR)
	}
	if h.isReservedWorkloadIP(parsed, network.IP) {
		return fmt.Errorf("workload address %s is reserved by maclet", ip)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return fmt.Errorf("PodCIDR %q does not have usable workload addresses", h.PodCIDR)
	}
	hostBits := bits - prefix
	broadcast := binary.BigEndian.Uint32(network.IP.To4()) + uint32(1<<hostBits) - 1
	if binary.BigEndian.Uint32(parsed) == broadcast {
		return fmt.Errorf("workload address %s is the PodCIDR broadcast address", ip)
	}
	canonicalIP := parsed.String()
	for _, existing := range h.Aliases {
		if existing == canonicalIP {
			return nil
		}
	}
	command := privilegedCommand(h.useSudo, "ifconfig", h.Interface, "inet", canonicalIP, "netmask", "255.255.255.255", "alias")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("add workload address %s to %s: %w (%s)", canonicalIP, h.Interface, err, strings.TrimSpace(string(output)))
	}
	h.Aliases = append(h.Aliases, canonicalIP)
	return nil
}

func (h *DarwinNetworkHandle) isReservedWorkloadIP(ip, networkIP net.IP) bool {
	if ip.Equal(networkIP) {
		return true
	}
	bridgeIP, _ := bridgeAddressForCIDR(h.PodCIDR)
	bridgeAddress := net.ParseIP(strings.Split(bridgeIP, "/")[0])
	if ip.Equal(bridgeAddress) || ip.Equal(net.ParseIP(h.Gateway)) {
		return true
	}
	for _, peer := range h.PeerGateways {
		if ip.Equal(net.ParseIP(peer.Gateway)) {
			return true
		}
	}
	return false
}

func (h *DarwinNetworkHandle) firstAvailableWorkloadIP(used map[string]bool) (string, error) {
	_, network, err := net.ParseCIDR(h.PodCIDR)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", h.PodCIDR, err)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 {
		return "", errors.New("only IPv4 PodCIDRs are currently supported")
	}
	broadcastOffset := uint32(1<<(bits-prefix)) - 1
	for offset := uint32(3); offset < broadcastOffset; offset++ {
		ip, err := workloadIPForOffset(h.PodCIDR, offset)
		if err != nil {
			return "", err
		}
		if !used[ip] && !h.isReservedWorkloadIP(net.ParseIP(ip), network.IP) {
			return ip, nil
		}
	}
	return "", fmt.Errorf("PodCIDR %q has no available workload addresses", h.PodCIDR)
}

func (h *DarwinNetworkHandle) removeWorkloadIP(ip string) error {
	canonicalIP := net.ParseIP(ip)
	if canonicalIP == nil || canonicalIP.To4() == nil {
		return fmt.Errorf("workload address %q is not an IPv4 address", ip)
	}
	canonicalIP = canonicalIP.To4()
	address := canonicalIP.String()
	index := -1
	for i, existing := range h.Aliases {
		if existing == address {
			index = i
			break
		}
	}
	if index == -1 {
		return nil
	}
	command := privilegedCommand(h.useSudo, "ifconfig", h.Interface, "inet", address, "-alias")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("remove workload address %s from %s: %w (%s)", address, h.Interface, err, strings.TrimSpace(string(output)))
	}
	h.Aliases = append(h.Aliases[:index], h.Aliases[index+1:]...)
	return nil
}

func (h *DarwinNetworkHandle) setGatewayMAC(mac string) error {
	parsed, err := net.ParseMAC(mac)
	if err != nil || len(parsed) != 6 {
		return fmt.Errorf("parse Flannel gateway MAC %q", mac)
	}
	mac = parsed.String()
	if h.GatewayMAC == mac {
		return nil
	}
	command := privilegedCommand(h.useSudo, "arp", "-S", h.Gateway, mac, "ifscope", h.Interface)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("update Darwin ARP gateway %s on %s: %w (%s)", h.Gateway, h.Interface, err, strings.TrimSpace(string(output)))
	}
	for index := range h.ARPs {
		if h.ARPs[index].IP == h.Gateway {
			h.ARPs[index].MAC = mac
			break
		}
	}
	h.GatewayMAC = mac
	return nil
}

func (h *DarwinNetworkHandle) cleanup() error {
	var cleanupErrors []error
	if _, err := net.InterfaceByName(h.Interface); err != nil {
		// vmnet can tear down the bridge before maclet's deferred cleanup runs.
		// Once the interface is gone, its aliases, routes, and scoped ARP entries
		// are gone with it; do not turn that normal shutdown race into a warning.
		h.Aliases = nil
		h.Routes = nil
		h.ARPs = nil
		return nil
	}
	for i := len(h.Aliases) - 1; i >= 0; i-- {
		if err := h.removeWorkloadIP(h.Aliases[i]); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	for i := len(h.Routes) - 1; i >= 0; i-- {
		route := h.Routes[i]
		command := privilegedCommand(h.useSudo, "route", "-n", "delete", "-net", route.Network, "-netmask", route.Netmask, route.Gateway)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete route %s/%s: %w (%s)", route.Network, route.Netmask, err, strings.TrimSpace(string(output))))
		}
	}
	for i := len(h.ARPs) - 1; i >= 0; i-- {
		arp := h.ARPs[i]
		command := privilegedCommand(h.useSudo, "arp", "-d", arp.IP, "ifscope", h.Interface)
		if output, err := command.CombinedOutput(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete ARP gateway %s: %w (%s)", arp.IP, err, strings.TrimSpace(string(output))))
		}
	}
	return errors.Join(cleanupErrors...)
}

func bridgeAddressForCIDR(cidr string) (string, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("parse PodCIDR %q: %w", cidr, err)
	}
	if ip4 := network.IP.To4(); ip4 != nil {
		value := binary.BigEndian.Uint32(ip4)
		if value == ^uint32(0) {
			return "", errors.New("PodCIDR has no usable first host address")
		}
		value++
		first := make(net.IP, net.IPv4len)
		binary.BigEndian.PutUint32(first, value)
		prefix, _ := network.Mask.Size()
		return fmt.Sprintf("%s/%d", first.String(), prefix), nil
	}
	return "", errors.New("only IPv4 PodCIDRs are currently supported by the Darwin VXLAN bridge")
}
