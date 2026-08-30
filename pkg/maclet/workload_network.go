package maclet

import (
	"errors"
	"fmt"
	"net"
)

func validateWorkloadAddress(cidr, ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return errors.New("address is not IPv4")
	}
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	if !network.Contains(parsed) {
		return fmt.Errorf("address is outside PodCIDR %s", cidr)
	}
	prefix, bits := network.Mask.Size()
	if bits != 32 || prefix > 30 {
		return errors.New("PodCIDR has no usable workload addresses")
	}
	value := uint32(0)
	for _, octet := range parsed.To4() {
		value = (value << 8) | uint32(octet)
	}
	networkValue := uint32(0)
	for _, octet := range network.IP.To4() {
		networkValue = (networkValue << 8) | uint32(octet)
	}
	hostBits := bits - prefix
	offset := value - networkValue
	broadcastOffset := uint32(1<<hostBits) - 1
	if offset < 3 || offset >= broadcastOffset {
		return errors.New("address is reserved or is the PodCIDR broadcast address")
	}
	return nil
}

func (h *DarwinNetworkHandle) validateWorkloadAddress(ip string) error {
	if err := validateWorkloadAddress(h.PodCIDR, ip); err != nil {
		return err
	}
	parsed := net.ParseIP(ip).To4()
	_, network, _ := net.ParseCIDR(h.PodCIDR)
	if h.isReservedWorkloadIP(parsed, network.IP) {
		return errors.New("address is reserved by maclet")
	}
	return nil
}

func (m *workloadManager) cleanup() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cleanupErrors []error
	journalChanged := false
	for uid, workload := range m.workloads {
		if err := m.removeWorkload(workload); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup workload %s: %w", uid, err))
			continue
		}
		delete(m.workloads, uid)
		journalChanged = true
	}
	if journalChanged || len(m.workloads) == 0 {
		if err := m.persistJournalLocked(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}
