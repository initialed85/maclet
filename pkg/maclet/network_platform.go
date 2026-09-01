package maclet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func signalProcessTree(pid int, useSudo bool, signal syscall.Signal) {
	children := processDescendants(pid)
	for index := len(children) - 1; index >= 0; index-- {
		child := children[index]
		if useSudo {
			_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", signal), fmt.Sprint(child)).Run()
		} else {
			_ = syscall.Kill(child, signal)
		}
	}
	if useSudo {
		// The root is usually the user-owned sudo wrapper, but using sudo here
		// also handles callers that pass the root-owned darwin-vxlan child.
		_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", signal), fmt.Sprint(pid)).Run()
	} else {
		_ = syscall.Kill(pid, signal)
	}
}

func processDescendants(pid int) []int {
	output, err := exec.Command("pgrep", "-P", fmt.Sprint(pid)).Output()
	if err != nil {
		return nil
	}
	children := make([]int, 0)
	for _, field := range strings.Fields(string(output)) {
		child, parseErr := strconv.Atoi(field)
		if parseErr != nil || child <= 0 {
			continue
		}
		children = append(children, processDescendants(child)...)
		children = append(children, child)
	}
	return children
}

func interfaceForAddress(addressCIDR string) (string, error) {
	ip, _, err := net.ParseCIDR(addressCIDR)
	if err != nil {
		return "", fmt.Errorf("parse interface address %q: %w", addressCIDR, err)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var addressIP net.IP
			switch value := address.(type) {
			case *net.IPNet:
				addressIP = value.IP
			case *net.IPAddr:
				addressIP = value.IP
			}
			if addressIP != nil && addressIP.Equal(ip) {
				return iface.Name, nil
			}
		}
	}
	return "", nil
}

func waitForBridge(ctx context.Context, bridgeCIDR string, timeout time.Duration, processDone <-chan struct{}) (string, string, error) {
	ip, _, err := net.ParseCIDR(bridgeCIDR)
	if err != nil {
		return "", "", fmt.Errorf("parse bridge address %q: %w", bridgeCIDR, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-processDone:
			return "", "", errors.New("darwin-vxlan exited before its bridge became ready")
		default:
		}
		interfaces, listErr := net.Interfaces()
		if listErr == nil {
			for _, iface := range interfaces {
				if len(iface.HardwareAddr) == 0 {
					continue
				}
				addresses, addrErr := iface.Addrs()
				if addrErr != nil {
					continue
				}
				for _, address := range addresses {
					var addressIP net.IP
					switch value := address.(type) {
					case *net.IPNet:
						addressIP = value.IP
					case *net.IPAddr:
						addressIP = value.IP
					}
					if addressIP != nil && addressIP.Equal(ip) {
						return iface.Name, iface.HardwareAddr.String(), nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("bridge address %s did not appear", bridgeCIDR)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
