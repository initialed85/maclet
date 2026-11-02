package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const (
	networksetupBinary = "networksetup"
	// `Empty` is the value networksetup accepts to clear search domains. `None` is
	// not: macOS stores it verbatim, so the host ends up with a search domain
	// literally called None, visible in `scutil --dns` as well as
	// `-getsearchdomains`. Clearing with `Empty` instead exposes the service's base
	// configuration again, which on a Mac that once ran a VPN client is that
	// client's leftover domain.
	networksetupNoSearchValue = "Empty"
)

// defaultSearchDomains is the tail of the Kubernetes search list a Pod receives.
// macOS attaches search domains to a network service rather than to a process, so
// maclet publishes one host-wide list. macOS applies those suffixes only to names
// without a dot and CoreDNS has no `<service>.svc.cluster.local` record, so a bare
// Service name needs the namespace entry too (for example
// `--dns-search-domain woodpecker.svc.cluster.local`); `svc.cluster.local` and
// `cluster.local` are what make fully qualified and `.svc` names land on the
// cluster resolver, and `cluster.local` last keeps the search list and the
// domain-scoped /etc/resolver/cluster.local file in agreement.
var defaultSearchDomains = []string{"svc.cluster.local", "cluster.local"}

type searchDomainRunner func(ctx context.Context, privileged bool, args ...string) ([]byte, error)

// clusterDNSSearchDomains publishes Kubernetes-style search domains on the network
// service that carries this node's underlay address. It complements the
// domain-scoped /etc/resolver/cluster.local file: macOS consults that file only for
// names under cluster.local, so a short name such as `woodpecker-server` otherwise
// resolves to nothing and the workload fails with an empty address list.
type clusterDNSSearchDomains struct {
	domains        []string
	device         string
	service        string
	useSudo        bool
	owned          bool
	foreign        []string
	foreignLogged  bool
	verified       bool
	resolvedDevice string
	runner         searchDomainRunner
	scutilRunner   searchDomainRunner
}

func newClusterDNSSearchDomains(useSudo bool, device, service string, domains ...string) *clusterDNSSearchDomains {
	managed := domains
	if len(managed) == 0 {
		managed = append([]string(nil), defaultSearchDomains...)
	}
	return &clusterDNSSearchDomains{
		domains:      normalizeSearchDomains(managed),
		device:       device,
		service:      strings.TrimSpace(service),
		useSudo:      useSudo,
		runner:       runNetworksetup,
		scutilRunner: runScutil,
	}
}

// effectiveSearchDomains reads the resolver chain macOS actually uses. A per
// network service setting is only honoured when nothing has installed a global
// DNS override (State:/Network/Global/DNS, which some VPN clients do), so the
// -getsearchdomains value alone is not proof that the search list is live.
func (s *clusterDNSSearchDomains) effectiveSearchDomains(ctx context.Context) ([]string, error) {
	output, err := s.scutilRunner(ctx, false, "--dns")
	if err != nil {
		return nil, fmt.Errorf("read macOS resolver chain: %w", err)
	}
	return parseEffectiveSearchDomains(output), nil
}

// verify reports whether every domain maclet wants appears in the unscoped
// resolver chain. It returns the effective list and whether maclet's own list is
// fully live.
func (s *clusterDNSSearchDomains) verify(ctx context.Context) ([]string, bool, error) {
	effective, err := s.effectiveSearchDomains(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, domain := range s.domains {
		found := false
		for _, candidate := range effective {
			if candidate == domain {
				found = true
				break
			}
		}
		if !found {
			return effective, false, nil
		}
	}
	return effective, true, nil
}

// reconcile gives the target network service maclet's search domains while
// preserving domains that belong to someone else. A service that already carries
// foreign search domains is treated as owned by that other manager (DHCP, a VPN
// client such as Tailscale) and maclet stays off it.
func (s *clusterDNSSearchDomains) reconcile(ctx context.Context) error {
	if len(s.domains) == 0 {
		return nil
	}
	if err := s.resolveService(ctx); err != nil {
		return err
	}
	current, err := s.searchDomains(ctx)
	if err != nil {
		return err
	}
	_, foreign := splitSearchDomains(current, s.domains)

	if len(foreign) > 0 {
		if s.owned {
			// Another manager added its own domains while maclet held the service.
			// Hand it back instead of fighting it on every heartbeat.
			if err := s.apply(ctx, foreign); err != nil {
				return err
			}
			s.owned = false
			s.foreign = nil
			log.Printf("search domains on %s now carry foreign entries (%s); maclet has released the service",
				s.service, strings.Join(foreign, ", "))
			return nil
		}
		if !s.foreignLogged {
			log.Printf("search domains on %s are already configured (%s) that maclet was not asked to publish (maclet would publish %s); pass --dns-search-domain for each domain you want maclet to own, or --dns-search-domains=false to leave them alone",
				s.service, strings.Join(foreign, ", "), strings.Join(s.domains, ", "))
			s.foreignLogged = true
		}
		return nil
	}

	desired := append([]string(nil), s.domains...)
	if s.owned && sameStrings(current, desired) {
		return nil
	}
	if err := s.apply(ctx, desired); err != nil {
		return err
	}
	if !s.owned {
		log.Printf("configured macOS search domains for %s (device %s): %s", s.service, s.logDevice(), strings.Join(desired, " "))
	}
	s.owned = true
	s.foreign = nil

	effective, live, verifyErr := s.verify(ctx)
	s.verified = live
	if verifyErr != nil {
		log.Printf("warning: cannot verify macOS search domains: %v", verifyErr)
		return nil
	}
	if !live {
		// Verification only runs when maclet actually wrote something, so this stays a
		// one-shot diagnostic rather than a per-heartbeat repeat.
		log.Printf("warning: macOS is not using the search domains maclet set on %s: %s carries %s; a global DNS override (State:/Network/Global/DNS, often a VPN client) shadows per-service settings, so workloads should use fully qualified cluster.local names or pass --dns-search-domains=false",
			s.service, s.service, strings.Join(effective, ", "))
	}
	return nil
}

// cleanup removes maclet's search domains and restores what was there before,
// including anything a foreign manager added while maclet owned the service.
func (s *clusterDNSSearchDomains) cleanup(ctx context.Context) error {
	if !s.owned {
		return nil
	}
	if err := s.resolveService(ctx); err != nil {
		return err
	}
	current, err := s.searchDomains(ctx)
	if err != nil {
		return err
	}
	_, foreign := splitSearchDomains(current, s.domains)
	restored := normalizeSearchDomains(append(append([]string(nil), s.foreign...), foreign...))
	if err := s.apply(ctx, restored); err != nil {
		return err
	}
	s.owned = false
	s.foreign = nil
	if len(restored) == 0 {
		log.Printf("cleared maclet search domains on %s", s.service)
	} else {
		log.Printf("cleared maclet search domains on %s, restored: %s", s.service, strings.Join(restored, " "))
	}
	return nil
}

func (s *clusterDNSSearchDomains) resolveService(ctx context.Context) error {
	if s.service != "" {
		return nil
	}
	if s.device == "" {
		return errors.New("search domain management needs a network device or an explicit --dns-search-domain-service")
	}
	output, err := s.runner(ctx, false, "-listnetworkserviceorder")
	if err != nil {
		return fmt.Errorf("list macOS network services: %w", err)
	}
	service := parseNetworkServiceDevice(output, s.device)
	if service == "" {
		return fmt.Errorf("no macOS network service owns device %s; pass --dns-search-domain-service explicitly", s.device)
	}
	s.service = service
	s.resolvedDevice = s.device
	return nil
}

func (s *clusterDNSSearchDomains) searchDomains(ctx context.Context) ([]string, error) {
	output, err := s.runner(ctx, false, "-getsearchdomains", s.service)
	if err != nil {
		return nil, fmt.Errorf("read macOS search domains for %s: %w", s.service, err)
	}
	return parseSearchDomains(output), nil
}

func (s *clusterDNSSearchDomains) apply(ctx context.Context, domains []string) error {
	output, err := s.runner(ctx, true, searchDomainsArgs(s.service, domains)...)
	if err != nil {
		return fmt.Errorf("set macOS search domains for %s: %w: %s", s.service, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *clusterDNSSearchDomains) logDevice() string {
	if s.resolvedDevice != "" {
		return s.resolvedDevice
	}
	if s.device != "" {
		return s.device
	}
	return "explicit service"
}

func runNetworksetup(ctx context.Context, privileged bool, args ...string) ([]byte, error) {
	return privilegedCommand(privileged, networksetupBinary, args...).CombinedOutput()
}

func runScutil(ctx context.Context, privileged bool, args ...string) ([]byte, error) {
	return privilegedCommand(privileged, "scutil", args...).CombinedOutput()
}

// parseEffectiveSearchDomains collects the search domains of the unscoped
// resolvers from `scutil --dns`, stopping at the scoped section so a VPN's
// interface-scoped ts.net entry is not mistaken for the system search list.
func parseEffectiveSearchDomains(output []byte) []string {
	candidates := make([]string, 0, 4)
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "DNS configuration (for scoped queries)") {
			break
		}
		if !strings.HasPrefix(line, "search domain[") {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		candidates = append(candidates, strings.TrimSpace(value))
	}
	return normalizeSearchDomains(candidates)
}

// normalizeSearchDomains lowercases, trims trailing dots, dedupes, and preserves
// order so a search list reads the same way in `scutil --dns` as it was requested.
func normalizeSearchDomains(domains []string) []string {
	normalized := make([]string, 0, len(domains))
	seen := make(map[string]bool, len(domains))
	for _, domain := range domains {
		domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if domain == "" || seen[domain] {
			continue
		}
		// Placeholders networksetup can echo back are not domains. Older maclet
		// releases wrote `None`, and macOS keeps it as a real search domain, so
		// ignore it rather than treating it as a foreign manager.
		if domain == "none" || domain == "empty" {
			continue
		}
		seen[domain] = true
		normalized = append(normalized, domain)
	}
	return normalized
}

func splitSearchDomains(current, managed []string) (ours, foreign []string) {
	managedSet := make(map[string]bool, len(managed))
	for _, domain := range managed {
		managedSet[domain] = true
	}
	for _, domain := range normalizeSearchDomains(current) {
		if managedSet[domain] {
			ours = append(ours, domain)
			continue
		}
		foreign = append(foreign, domain)
	}
	return ours, foreign
}

func searchDomainsArgs(service string, domains []string) []string {
	args := []string{"-setsearchdomains", service}
	if len(domains) == 0 {
		return append(args, networksetupNoSearchValue)
	}
	return append(args, domains...)
}

// parseNetworkServiceDevice reads `networksetup -listnetworkserviceorder`, which
// pairs a service name with its hardware port and device:
//
//	(5) USB 10/100/1000 LAN
//	(Hardware Port: AX88179, Device: en13)
//
// The service name is what -getsearchdomains and -setsearchdomains accept and it
// is not always the hardware port name. Disabled services (asterisk marker) are
// skipped.
func parseNetworkServiceDevice(output []byte, device string) string {
	service := ""
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "(Hardware Port:") {
			if strings.HasSuffix(line, "Device: "+device+")") {
				return service
			}
			service = ""
			continue
		}
		if !strings.HasPrefix(line, "(") {
			if strings.HasPrefix(line, "*") {
				// Disabled service, for example "*(2) AirPort"; it cannot carry
				// search domains, so drop the pending service name.
				service = ""
			}
			continue
		}
		rest := line[1:]
		end := strings.Index(rest, ")")
		if end < 0 {
			continue
		}
		marker := rest[:end]
		name := strings.TrimSpace(rest[end+1:])
		if name == "" || strings.Contains(marker, "*") {
			service = ""
			continue
		}
		service = name
	}
	return ""
}

// parseSearchDomains reads `networksetup -getsearchdomains <service>` output, which
// is either one domain per line or the sentence
// "There aren't any Search Domains set on <service>.".
func parseSearchDomains(output []byte) []string {
	candidates := make([]string, 0, 4)
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.Contains(line, "There aren't any Search Domains") {
			continue
		}
		candidates = append(candidates, line)
	}
	return normalizeSearchDomains(candidates)
}

// underlayDeviceForIP reports the network device holding an underlay address, so
// search domains land on the interface that actually carries cluster traffic.
func underlayDeviceForIP(address string) (string, error) {
	if strings.TrimSpace(address) == "" {
		return "", errors.New("underlay address is empty")
	}
	device, err := interfaceForAddress(address + "/32")
	if err != nil {
		return "", fmt.Errorf("find device for underlay address %s: %w", address, err)
	}
	if device == "" {
		return "", fmt.Errorf("no network device holds underlay address %s", address)
	}
	return device, nil
}

// searchDomainUnderlayIP picks the underlay address whose device should carry the
// search domains. VXLAN local wins because that is the address cluster traffic
// actually leaves from.
func searchDomainUnderlayIP(cfg JoinConfig, state *LocalState) string {
	candidates := []string{cfg.VXLANLocal, cfg.NodeIP, cfg.ExternalIP}
	if state != nil {
		candidates = append(candidates, state.NodeIP)
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

const searchDomainsStateFile = "search-domains.json"

// searchDomainsState is what a running join recorded about the service it touched,
// so `maclet leave` can undo exactly that instead of guessing at defaults.
type searchDomainsState struct {
	Service string   `json:"service"`
	Device  string   `json:"device"`
	Domains []string `json:"domains"`
}

func searchDomainsStatePath(stateDir string) string {
	return filepath.Join(stateDir, searchDomainsStateFile)
}

func writeSearchDomainsState(stateDir string, s *clusterDNSSearchDomains) error {
	if stateDir == "" || s == nil {
		return nil
	}
	record := searchDomainsState{Service: s.service, Device: s.resolvedDevice, Domains: s.domains}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(searchDomainsStatePath(stateDir), append(body, '\n'), 0600)
}

func readSearchDomainsState(stateDir string) (*searchDomainsState, error) {
	body, err := os.ReadFile(searchDomainsStatePath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", searchDomainsStatePath(stateDir), err)
	}
	var record searchDomainsState
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("decode %s: %w", searchDomainsStatePath(stateDir), err)
	}
	return &record, nil
}

func removeSearchDomainsState(stateDir string) error {
	if err := os.Remove(searchDomainsStatePath(stateDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// cleanupLeaveSearchDomains removes maclet's search domains after a join that this
// process did not make. The recorded state from the join is preferred; otherwise
// the underlay device is resolved from the saved NodeIP and the default list is
// assumed. Domains belonging to another manager are always kept.
func cleanupLeaveSearchDomains(ctx context.Context, useSudo bool, nodeIP, stateDir string) error {
	record, err := readSearchDomainsState(stateDir)
	if err != nil {
		return err
	}
	search := newClusterDNSSearchDomains(useSudo, "", "")
	if record != nil {
		search.service = strings.TrimSpace(record.Service)
		search.device = strings.TrimSpace(record.Device)
		if len(record.Domains) > 0 {
			search.domains = normalizeSearchDomains(record.Domains)
		}
	}
	if search.service == "" {
		device, deviceErr := underlayDeviceForIP(nodeIP)
		if deviceErr != nil {
			return deviceErr
		}
		search.device = device
	}
	if err := search.resolveService(ctx); err != nil {
		return err
	}
	current, err := search.searchDomains(ctx)
	if err != nil {
		return err
	}
	ours, foreign := splitSearchDomains(current, search.domains)
	if len(ours) == 0 {
		return removeSearchDomainsState(stateDir)
	}
	if err := search.apply(ctx, foreign); err != nil {
		return err
	}
	log.Printf("cleared maclet search domains on %s", search.service)
	return removeSearchDomainsState(stateDir)
}
