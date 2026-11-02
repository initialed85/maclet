package maclet

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const networkServiceOrderFixture = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

*(2) USB JTAG/serial debug unit 2
(Hardware Port: USB JTAG/serial debug unit, Device: usbmodem1101)

(3) AX88179
(Hardware Port: AX88179, Device: en13)

(4) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)
`

func TestParseNetworkServiceDevice(t *testing.T) {
	cases := []struct {
		device string
		want   string
	}{
		{device: "en13", want: "AX88179"},
		{device: "en0", want: "Wi-Fi"},
		{device: "bridge0", want: "Thunderbolt Bridge"},
		{device: "usbmodem1101", want: ""},
		{device: "en99", want: ""},
	}
	for _, tc := range cases {
		if got := parseNetworkServiceDevice([]byte(networkServiceOrderFixture), tc.device); got != tc.want {
			t.Errorf("device %s: got %q want %q", tc.device, got, tc.want)
		}
	}
}

func TestParseSearchDomains(t *testing.T) {
	if got := parseSearchDomains([]byte("There aren't any Search Domains set on USB 10/100/1000 LAN.\n")); len(got) != 0 {
		t.Fatalf("empty sentence parsed as %v", got)
	}
	got := parseSearchDomains([]byte("SVC.Cluster.Local.\ncluster.local\n"))
	want := []string{"svc.cluster.local", "cluster.local"}
	if !sameStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNormalizeSearchDomains(t *testing.T) {
	got := normalizeSearchDomains([]string{"Cluster.Local", " cluster.local ", "", "svc.cluster.local.", "Cluster.Local"})
	want := []string{"cluster.local", "svc.cluster.local"}
	if !sameStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSplitSearchDomains(t *testing.T) {
	ours, foreign := splitSearchDomains([]string{"svc.cluster.local", "ts.net", "cluster.local"}, defaultSearchDomains)
	if !sameStrings(ours, []string{"svc.cluster.local", "cluster.local"}) {
		t.Errorf("ours: got %v", ours)
	}
	if !sameStrings(foreign, []string{"ts.net"}) {
		t.Errorf("foreign: got %v", foreign)
	}
}

func TestSearchDomainsArgsClearsServiceWhenEmpty(t *testing.T) {
	args := searchDomainsArgs("AX88179", nil)
	want := []string{"-setsearchdomains", "AX88179", networksetupNoSearchValue}
	if !sameStrings(args, want) {
		t.Fatalf("got %v want %v", args, want)
	}
}

type fakeSearchDomainRunner struct {
	current  []string
	shadowed []string
	calls    [][]string
	reads    int
	setPriv  bool
}

func (f *fakeSearchDomainRunner) run(ctx context.Context, privileged bool, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "-listnetworkserviceorder":
		return []byte(networkServiceOrderFixture), nil
	case "-getsearchdomains":
		f.reads++
		if len(f.current) == 0 {
			return []byte("There aren't any Search Domains set on USB 10/100/1000 LAN.\n"), nil
		}
		return []byte(strings.Join(f.current, "\n") + "\n"), nil
	case "-setsearchdomains":
		if !privileged {
			return nil, fmt.Errorf("setting search domains needs root")
		}
		f.setPriv = true
		domains := args[2:]
		if len(domains) == 1 && domains[0] == networksetupNoSearchValue {
			f.current = nil
		} else {
			f.current = append([]string(nil), domains...)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected networksetup arguments: %v", args)
}

func (f *fakeSearchDomainRunner) scutil(ctx context.Context, privileged bool, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "--dns" {
		return nil, fmt.Errorf("unexpected scutil arguments: %v", args)
	}
	var out strings.Builder
	out.WriteString("DNS configuration\n\nresolver #1\n")
	if f.shadowed != nil {
		for index, domain := range f.shadowed {
			fmt.Fprintf(&out, "  search domain[%d] : %s\n", index, domain)
		}
		out.WriteString("  nameserver[0] : 192.168.137.1\n  if_index : 24 (en13)\n")
		out.WriteString("\nresolver #2\n  domain   : local\n  options  : mdns\n")
		out.WriteString("\nDNS configuration (for scoped queries)\n\nresolver #1\n  search domain[0] : ts.net\n  if_index : 24 (en13)\n")
		return []byte(out.String()), nil
	}
	for index, domain := range f.current {
		fmt.Fprintf(&out, "  search domain[%d] : %s\n", index, domain)
	}
	out.WriteString("  nameserver[0] : 10.43.0.10\n  if_index : 24 (en13)\n")
	out.WriteString("\nDNS configuration (for scoped queries)\n")
	return []byte(out.String()), nil
}

func (f *fakeSearchDomainRunner) sets() [][]string {
	sets := make([][]string, 0, len(f.calls))
	for _, call := range f.calls {
		if call[0] == "-setsearchdomains" {
			sets = append(sets, call)
		}
	}
	return sets
}

func newTestSearchDomains() (*clusterDNSSearchDomains, *fakeSearchDomainRunner) {
	search := newClusterDNSSearchDomains(true, "en13", "")
	fake := &fakeSearchDomainRunner{}
	search.runner = fake.run
	search.scutilRunner = fake.scutil
	return search, fake
}

func TestClusterDNSSearchDomainsReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	search, fake := newTestSearchDomains()

	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !search.owned {
		t.Fatalf("expected maclet to own the service")
	}
	sets := fake.sets()
	if len(sets) != 1 {
		t.Fatalf("expected one set call, got %d: %v", len(sets), sets)
	}
	want := []string{"-setsearchdomains", "AX88179", "svc.cluster.local", "cluster.local"}
	if !sameStrings(sets[0], want) {
		t.Fatalf("got %v want %v", sets[0], want)
	}
	if search.service != "AX88179" || search.resolvedDevice != "en13" {
		t.Fatalf("resolved service/device: %q %q", search.service, search.resolvedDevice)
	}
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(fake.sets()) != 1 {
		t.Fatalf("reconcile rewrote search domains it already owned: %v", fake.sets())
	}
}

func TestClusterDNSSearchDomainsLeaveForeignServiceAlone(t *testing.T) {
	ctx := context.Background()
	search, fake := newTestSearchDomains()
	fake.current = []string{"ts.net"}

	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if search.owned {
		t.Fatalf("maclet claimed a service that already had foreign search domains")
	}
	if sets := fake.sets(); len(sets) != 0 {
		t.Fatalf("maclet modified a foreign service: %v", sets)
	}
	if err := search.cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(fake.sets()) != 0 {
		t.Fatalf("cleanup modified a foreign service: %v", fake.sets())
	}
}

func TestClusterDNSSearchDomainsReleaseServiceWhenForeignEntryAppears(t *testing.T) {
	ctx := context.Background()
	search, fake := newTestSearchDomains()
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	fake.current = []string{"svc.cluster.local", "cluster.local", "ts.net"}
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if search.owned {
		t.Fatalf("maclet should release a service a VPN client joined")
	}
	sets := fake.sets()
	want := []string{"-setsearchdomains", "AX88179", "ts.net"}
	if !sameStrings(sets[len(sets)-1], want) {
		t.Fatalf("last set call: got %v want %v", sets[len(sets)-1], want)
	}
	if err := search.cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(fake.sets()) != len(sets) {
		t.Fatalf("cleanup touched the service after releasing it: %v", fake.sets())
	}
}

func TestClusterDNSSearchDomainsCleanupRestoresPreviousState(t *testing.T) {
	ctx := context.Background()
	search, fake := newTestSearchDomains()
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := search.cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	sets := fake.sets()
	want := []string{"-setsearchdomains", "AX88179", networksetupNoSearchValue}
	if !sameStrings(sets[len(sets)-1], want) {
		t.Fatalf("last set call: got %v want %v", sets[len(sets)-1], want)
	}
	if search.owned {
		t.Fatalf("cleanup left maclet marked as owner")
	}
}

func TestClusterDNSSearchDomainsCleanupKeepsForeignDomains(t *testing.T) {
	ctx := context.Background()
	search, fake := newTestSearchDomains()
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	fake.current = []string{"svc.cluster.local", "cluster.local", "corp.example"}
	if err := search.cleanup(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	sets := fake.sets()
	want := []string{"-setsearchdomains", "AX88179", "corp.example"}
	if !sameStrings(sets[len(sets)-1], want) {
		t.Fatalf("last set call: got %v want %v", sets[len(sets)-1], want)
	}
}

func TestClusterDNSSearchDomainsUsesExplicitServiceOverride(t *testing.T) {
	ctx := context.Background()
	search := newClusterDNSSearchDomains(true, "", "Thunderbolt Bridge")
	fake := &fakeSearchDomainRunner{}
	search.runner = fake.run
	search.scutilRunner = fake.scutil
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, call := range fake.calls {
		if call[0] == "-listnetworkserviceorder" {
			t.Fatalf("explicit --dns-search-domain-service still listed services: %v", fake.calls)
		}
	}
	if search.service != "Thunderbolt Bridge" {
		t.Fatalf("service: %q", search.service)
	}
}

func TestClusterDNSSearchDomainsHonoursCustomDomains(t *testing.T) {
	ctx := context.Background()
	search := newClusterDNSSearchDomains(true, "en13", "", "lab.internal", "cluster.local")
	fake := &fakeSearchDomainRunner{}
	search.runner = fake.run
	search.scutilRunner = fake.scutil
	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	sets := fake.sets()
	want := []string{"-setsearchdomains", "AX88179", "lab.internal", "cluster.local"}
	if !sameStrings(sets[0], want) {
		t.Fatalf("got %v want %v", sets[0], want)
	}
}

func TestClusterDNSSearchDomainsReportsShadowedSearchDomains(t *testing.T) {
	ctx := context.Background()
	search := newClusterDNSSearchDomains(true, "en13", "")
	fake := &fakeSearchDomainRunner{shadowed: []string{"ts.net"}}
	search.runner = fake.run
	search.scutilRunner = fake.scutil

	if err := search.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !search.owned {
		t.Fatalf("maclet should still own the service it wrote to")
	}
	if search.verified {
		t.Fatalf("maclet claimed the search domains were live while a global override shadowed them")
	}
	for range [4]int{} {
		if err := search.reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if sets := fake.sets(); len(sets) != 1 {
		t.Fatalf("shadowed search domains caused rewrite churn: %v", sets)
	}
}

func TestParseEffectiveSearchDomainsIgnoresScopedResolvers(t *testing.T) {
	fake := &fakeSearchDomainRunner{shadowed: []string{"ts.net"}}
	output, err := fake.scutil(context.Background(), false, "--dns")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got := parseEffectiveSearchDomains(output)
	if !sameStrings(got, []string{"ts.net"}) {
		t.Fatalf("got %v", got)
	}
	if len(parseEffectiveSearchDomains([]byte("DNS configuration\n\nresolver #1\n  nameserver[0] : 1.1.1.1\n"))) != 0 {
		t.Fatalf("expected no search domains")
	}
}

func TestSearchDomainsStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	search, _ := newTestSearchDomains()
	if search.service == "" {
		search.service = "AX88179"
		search.resolvedDevice = "en13"
	}
	if err := writeSearchDomainsState(dir, search); err != nil {
		t.Fatalf("write state: %v", err)
	}
	record, err := readSearchDomainsState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if record == nil || record.Service != "AX88179" || record.Device != "en13" {
		t.Fatalf("record: %+v", record)
	}
	if !sameStrings(record.Domains, defaultSearchDomains) {
		t.Fatalf("record.Domains: %v", record.Domains)
	}
	if err := removeSearchDomainsState(dir); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	missing, err := readSearchDomainsState(dir)
	if err != nil || missing != nil {
		t.Fatalf("after removal: %+v %v", missing, err)
	}
}
