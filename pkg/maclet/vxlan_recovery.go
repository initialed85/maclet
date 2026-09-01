package maclet

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type matchingVXLANProcess struct {
	PID        int
	Command    string
	BridgeCIDR string
	Wrapper    bool
}

// findMatchingVXLANProcesses finds darwin-vxlan processes that have the same
// local/remote Flannel transport identity as this join. The bridge CIDR is not
// part of the identity because a node may receive a different PodCIDR after a
// previous unclean registration; such a process is still an orphan owned by
// the one maclet instance on this host.
func findMatchingVXLANProcesses(binary, local, remote string, port int) ([]matchingVXLANProcess, error) {
	output, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	processes := make([]matchingVXLANProcess, 0)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		command := strings.Join(fields[1:], " ")
		bridgeCIDR, ok := matchingVXLANInvocation(command, binary, local, remote, port)
		if !ok {
			continue
		}
		processes = append(processes, matchingVXLANProcess{
			PID: pid, Command: command, BridgeCIDR: bridgeCIDR,
			Wrapper: fields[1] == "sudo",
		})
	}
	return processes, nil
}

func matchingVXLANInvocation(command, binary, local, remote string, port int) (string, bool) {
	fields := strings.Fields(command)
	binaryBase := filepath.Base(binary)
	binaryIndex := -1
	for index, field := range fields {
		if field == binary || (binaryBase == "darwin-vxlan" && filepath.Base(field) == binaryBase) {
			binaryIndex = index
			break
		}
	}
	if binaryIndex < 0 {
		return "", false
	}
	if flagValue(fields, "--vni") != "1" ||
		flagValue(fields, "--local") != local ||
		flagValue(fields, "--remote") != remote ||
		flagValue(fields, "--port") != strconv.Itoa(port) {
		return "", false
	}
	bridgeCIDR := flagValue(fields, "--bridge-ipv4")
	if bridgeCIDR == "" {
		return "", false
	}
	if _, _, err := net.ParseCIDR(bridgeCIDR); err != nil {
		return "", false
	}
	return bridgeCIDR, true
}

func flagValue(fields []string, flag string) string {
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == flag {
			return fields[index+1]
		}
	}
	return ""
}

// recoverStaleVXLAN removes network state that belongs to a previous maclet
// tunnel before a new child is launched. It is deliberately narrow: only a
// bridge carrying the requested bridge address or a darwin-vxlan invocation
// matching this node's transport identity is considered ours.
func recoverStaleVXLAN(cfg JoinConfig, bridgeCIDR, local string) (bool, error) {
	processes, err := findMatchingVXLANProcesses(cfg.VXLANBinary, local, cfg.VXLANRemote, cfg.VXLANPort)
	if err != nil {
		return false, err
	}
	interfaces := make(map[string][]string)
	currentInterface, err := interfaceForAddress(bridgeCIDR)
	if err != nil {
		return false, err
	}
	if currentInterface != "" {
		if !isBridgeInterface(currentInterface) {
			return false, fmt.Errorf("VXLAN bridge address %s is already present on non-bridge interface %s", bridgeCIDR, currentInterface)
		}
		interfaces[currentInterface] = append(interfaces[currentInterface], bridgeCIDR)
	}
	for _, process := range processes {
		if process.BridgeCIDR == "" {
			continue
		}
		iface, ifaceErr := interfaceForAddress(process.BridgeCIDR)
		if ifaceErr != nil {
			return false, fmt.Errorf("inspect stale VXLAN bridge %s: %w", process.BridgeCIDR, ifaceErr)
		}
		if iface != "" {
			if !isBridgeInterface(iface) {
				return false, fmt.Errorf("stale VXLAN address %s is on non-bridge interface %s", process.BridgeCIDR, iface)
			}
			interfaces[iface] = append(interfaces[iface], process.BridgeCIDR)
		}
	}
	if len(processes) == 0 && len(interfaces) == 0 {
		return false, nil
	}
	if len(processes) > 0 {
		if err := terminateMatchingVXLANProcesses(processes, cfg.useSudo, cfg.VXLANBinary, local, cfg.VXLANRemote, cfg.VXLANPort); err != nil {
			return true, err
		}
	}
	for iface, cidrs := range interfaces {
		if err := removeIPv4AddressesInCIDRs(iface, cfg.useSudo, cidrs); err != nil {
			return true, fmt.Errorf("remove stale VXLAN addresses from %s: %w", iface, err)
		}
	}
	if existing, checkErr := interfaceForAddress(bridgeCIDR); checkErr != nil {
		return true, checkErr
	} else if existing != "" {
		return true, fmt.Errorf("stale VXLAN bridge address %s remains on %s", bridgeCIDR, existing)
	}
	return true, nil
}

func isBridgeInterface(name string) bool {
	return strings.HasPrefix(name, "bridge")
}

func removeIPv4AddressesInCIDRs(ifaceName string, useSudo bool, cidrs []string) error {
	networks := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("parse stale address CIDR %q: %w", cidr, err)
		}
		networks = append(networks, network)
	}
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "no such network interface") {
			return nil
		}
		return err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return err
	}
	for _, address := range addresses {
		ip := addressIP(address)
		if ip == nil || ip.To4() == nil {
			continue
		}
		ip = ip.To4()
		owned := false
		for _, network := range networks {
			if network.Contains(ip) {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		command := privilegedCommand(useSudo, "ifconfig", iface.Name, "inet", ip.String(), "-alias")
		if output, removeErr := command.CombinedOutput(); removeErr != nil {
			return fmt.Errorf("remove IPv4 address %s: %w (%s)", ip, removeErr, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func addressIP(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	default:
		return nil
	}
}

func terminateMatchingVXLANProcesses(processes []matchingVXLANProcess, useSudo bool, binary, local, remote string, port int) error {
	for _, process := range processes {
		if process.Wrapper {
			signalProcessTree(process.PID, useSudo, syscall.SIGINT)
			continue
		}
		if useSudo {
			_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", syscall.SIGINT), strconv.Itoa(process.PID)).Run()
		} else {
			_ = syscall.Kill(process.PID, syscall.SIGINT)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining, err := findMatchingVXLANProcesses(binary, local, remote, port)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			for _, process := range remaining {
				if useSudo {
					_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", syscall.SIGKILL), strconv.Itoa(process.PID)).Run()
				} else {
					_ = syscall.Kill(process.PID, syscall.SIGKILL)
				}
			}
			return fmt.Errorf("stale darwin-vxlan process(es) did not stop: %s", formatProcessPIDs(remaining))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func formatProcessPIDs(processes []matchingVXLANProcess) string {
	pids := make([]string, 0, len(processes))
	for _, process := range processes {
		pids = append(pids, strconv.Itoa(process.PID))
	}
	return strings.Join(pids, ",")
}
