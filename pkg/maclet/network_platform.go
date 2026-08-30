package maclet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func signalProcessTree(pid int, useSudo bool, signal syscall.Signal) {
	if useSudo {
		if output, err := exec.Command("pgrep", "-P", fmt.Sprint(pid)).Output(); err == nil {
			for _, child := range strings.Fields(string(output)) {
				_ = privilegedCommand(true, "kill", fmt.Sprintf("-%d", signal), child).Run()
			}
		}
	}
	_ = syscall.Kill(pid, signal)
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
