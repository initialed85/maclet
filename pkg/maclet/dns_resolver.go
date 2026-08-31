package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultResolverDomain       = "cluster.local"
	defaultResolverPath         = "/etc/resolver/" + defaultResolverDomain
	defaultClusterDNSNamespace  = "kube-system"
	defaultClusterDNSService    = "kube-dns"
	resolverManagedComment      = "# Managed by maclet; do not edit."
	resolverDomainCommentPrefix = "# Cluster DNS domain: "
)

type clusterDNSResolver struct {
	path        string
	domain      string
	useSudo     bool
	nameservers []string
	owned       bool
}

func newClusterDNSResolver(useSudo bool) *clusterDNSResolver {
	return &clusterDNSResolver{
		path:    defaultResolverPath,
		domain:  defaultResolverDomain,
		useSudo: useSudo,
	}
}

func (r *clusterDNSResolver) reconcile(ctx context.Context, client *APIClient) error {
	nameservers, err := discoverClusterDNSNameservers(ctx, client)
	if err != nil {
		return err
	}
	if sameStrings(r.nameservers, nameservers) && r.owned {
		matches, matchErr := managedResolverFileMatches(r.path, r.domain, nameservers)
		if matchErr != nil {
			return matchErr
		}
		if matches {
			return nil
		}
	}
	if err := r.write(ctx, nameservers); err != nil {
		return err
	}
	r.nameservers = nameservers
	r.owned = true
	return nil
}

func (r *clusterDNSResolver) write(ctx context.Context, nameservers []string) error {
	if r.useSudo {
		return runResolverHelper(ctx, false, nameservers)
	}
	return writeManagedResolverFile(r.path, r.domain, nameservers)
}

func (r *clusterDNSResolver) cleanup(ctx context.Context) error {
	if !r.owned {
		return nil
	}
	if r.useSudo {
		if err := runResolverHelper(ctx, true, nil); err != nil {
			return err
		}
	} else if err := removeManagedResolverFile(r.path); err != nil {
		return err
	}
	r.owned = false
	r.nameservers = nil
	return nil
}

func discoverClusterDNSNameservers(ctx context.Context, client *APIClient) ([]string, error) {
	if client == nil {
		return nil, errors.New("cluster DNS discovery requires a peer Kubernetes client")
	}
	path := "/api/v1/namespaces/" + defaultClusterDNSNamespace + "/services/" + defaultClusterDNSService
	body, err := client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("get cluster DNS Service %s/%s: %w", defaultClusterDNSNamespace, defaultClusterDNSService, err)
	}
	var service Service
	if err := json.Unmarshal(body, &service); err != nil {
		return nil, fmt.Errorf("decode cluster DNS Service: %w", err)
	}
	candidates := service.Spec.ClusterIPs
	if len(candidates) == 0 && service.Spec.ClusterIP != "" {
		candidates = []string{service.Spec.ClusterIP}
	}
	nameservers := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "None" || net.ParseIP(candidate) == nil || seen[candidate] {
			continue
		}
		seen[candidate] = true
		nameservers = append(nameservers, candidate)
	}
	if len(nameservers) == 0 {
		return nil, fmt.Errorf("cluster DNS Service %s/%s has no usable ClusterIP", defaultClusterDNSNamespace, defaultClusterDNSService)
	}
	sort.Strings(nameservers)
	return nameservers, nil
}

func writeManagedResolverFile(path, domain string, nameservers []string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("resolver path must be absolute")
	}
	if domain == "" {
		return errors.New("resolver domain is required")
	}
	if len(nameservers) == 0 {
		return errors.New("at least one resolver nameserver is required")
	}
	for _, nameserver := range nameservers {
		if net.ParseIP(nameserver) == nil {
			return fmt.Errorf("invalid resolver nameserver %q", nameserver)
		}
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0755); err != nil {
		return fmt.Errorf("create resolver directory: %w", err)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if !isManagedResolverFile(existing) {
			return fmt.Errorf("resolver file %s exists and is not managed by maclet", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read resolver file %s: %w", path, err)
	}

	content := resolverFileContent(domain, nameservers)

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary resolver file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0644); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set resolver file permissions: %w", err)
	}
	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write resolver file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync resolver file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close resolver file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install resolver file: %w", err)
	}
	return nil
}

func managedResolverFileMatches(path, domain string, nameservers []string) (bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read resolver file %s: %w", path, err)
	}
	if !isManagedResolverFile(body) {
		return false, nil
	}
	return string(body) == resolverFileContent(domain, nameservers), nil
}

func resolverFileContent(domain string, nameservers []string) string {
	var content strings.Builder
	content.WriteString(resolverManagedComment)
	content.WriteByte('\n')
	content.WriteString(resolverDomainCommentPrefix)
	content.WriteString(domain)
	content.WriteByte('\n')
	for _, nameserver := range nameservers {
		content.WriteString("nameserver ")
		content.WriteString(nameserver)
		content.WriteByte('\n')
	}
	return content.String()
}

func removeManagedResolverFile(path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read resolver file %s: %w", path, err)
	}
	if !isManagedResolverFile(body) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove resolver file %s: %w", path, err)
	}
	return nil
}

func isManagedResolverFile(body []byte) bool {
	firstLine := strings.SplitN(string(body), "\n", 2)[0]
	return strings.TrimSpace(firstLine) == resolverManagedComment
}

func runResolverHelper(ctx context.Context, remove bool, nameservers []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate maclet executable: %w", err)
	}
	args := []string{"resolver-helper"}
	if remove {
		args = append(args, "--remove")
	} else {
		for _, nameserver := range nameservers {
			args = append(args, "--nameserver", nameserver)
		}
	}
	command := privilegedCommand(true, executable, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run privileged resolver helper: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func resolverHelperCommand(args []string) error {
	flags := flag.NewFlagSet("resolver-helper", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	remove := false
	var nameservers stringSliceFlag
	flags.BoolVar(&remove, "remove", false, "remove the managed resolver file")
	flags.Var(&nameservers, "nameserver", "cluster DNS nameserver IP (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if remove {
		if len(nameservers) != 0 {
			return errors.New("--remove cannot be combined with --nameserver")
		}
		return removeManagedResolverFile(defaultResolverPath)
	}
	return writeManagedResolverFile(defaultResolverPath, defaultResolverDomain, nameservers)
}

type stringSliceFlag []string

func (f *stringSliceFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringSliceFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
