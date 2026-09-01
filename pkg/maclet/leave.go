package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	leaveNodePasswordNamespace = "kube-system"
	leaveNodePasswordSuffix    = ".node-password.k3s"
)

type leaveConfig struct {
	StateDir    string
	NodeName    string
	Kubeconfig  string
	Context     string
	InsecureTLS bool
}

func readLocalState(stateDir string) (*LocalState, error) {
	statePath := filepath.Join(stateDir, "state.json")
	body, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read maclet state %s: %w (run maclet join first)", statePath, err)
	}
	var state LocalState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("decode maclet state %s: %w", statePath, err)
	}
	if state.Server == "" || state.NodeName == "" {
		return nil, fmt.Errorf("maclet state %s is incomplete", statePath)
	}
	return &state, nil
}

func leavePeerAPIClient(cfg leaveConfig, state *LocalState) (*APIClient, error) {
	path := cfg.Kubeconfig
	if path == "" {
		path = state.PeerKubeconfig
	}
	if path == "" {
		path = defaultPeerKubeconfig()
	}
	contextName := cfg.Context
	if contextName == "" {
		contextName = state.PeerContext
	}
	client, found, err := loadPeerAPIClient(state.Server, path, contextName, cfg.InsecureTLS)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("leave needs a kubeconfig with permission to remove Node %q, its Lease, and its node-password Secret; pass --kubeconfig", state.NodeName)
	}
	return client, nil
}

func leaveResource(ctx context.Context, client *APIClient, description, path string) error {
	if _, err := client.Delete(ctx, path); err != nil {
		var apiErr *HTTPError
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete %s: %w", description, err)
	}
	return nil
}

func leaveClusterResources(ctx context.Context, client *APIClient, nodeName string) error {
	nodePath := "/api/v1/nodes/" + url.PathEscape(nodeName)
	var cleanupErrors []error
	// Remove maclet's Flannel annotations while the Node still exists. This is
	// best effort: deleting the Node below is still useful if the patch races a
	// controller or the annotations are already absent.
	if err := clearFlannel(ctx, client, nodeName); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("clear Flannel annotations: %w", err))
	}
	if err := leaveResource(ctx, client, "kube-node-lease Lease", "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/"+url.PathEscape(nodeName)); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := leaveResource(ctx, client, "Node password Secret", "/api/v1/namespaces/"+leaveNodePasswordNamespace+"/secrets/"+url.PathEscape(nodeName+leaveNodePasswordSuffix)); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := leaveResource(ctx, client, "Node", nodePath); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	return errors.Join(cleanupErrors...)
}

func leaveSudoAvailable() bool {
	if os.Geteuid() == 0 {
		return false
	}
	return exec.Command("sudo", "-n", "true").Run() == nil
}

func cleanupLeaveResolver(ctx context.Context, useSudo bool) error {
	if useSudo {
		return runResolverHelper(ctx, true, nil)
	}
	return removeManagedResolverFile(defaultResolverPath)
}

func removeLocalStateFiles(stateDir string) error {
	files := []string{
		"state.json",
		"workloads.json",
		"server-ca.crt",
		"client-kubelet.crt",
		"client-kubelet.key",
		"client-k3s-controller.crt",
		"client-k3s-controller.key",
		"client-ca.crt",
		"serving-kubelet.crt",
		"serving-kubelet.key",
		"node-password",
	}
	var removeErrors []error
	for _, name := range files {
		path := filepath.Join(stateDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErrors = append(removeErrors, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	if err := os.Remove(stateDir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		removeErrors = append(removeErrors, fmt.Errorf("remove state directory %s: %w", stateDir, err))
	}
	return errors.Join(removeErrors...)
}

func runLeave(ctx context.Context, cfg leaveConfig) error {
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStatePath()
	}
	state, err := readLocalState(cfg.StateDir)
	if err != nil {
		return err
	}
	if cfg.NodeName != "" && cfg.NodeName != state.NodeName {
		return fmt.Errorf("state belongs to node %s, not %s", state.NodeName, cfg.NodeName)
	}
	client, err := leavePeerAPIClient(cfg, state)
	if err != nil {
		return fmt.Errorf("load leave kubeconfig: %w", err)
	}

	var cleanupErrors []error
	cleanupContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := cleanupLeaveResolver(cleanupContext, leaveSudoAvailable()); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("remove macOS cluster DNS resolver: %w", err))
	}
	if err := leaveClusterResources(cleanupContext, client, state.NodeName); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if len(cleanupErrors) != 0 {
		return errors.Join(cleanupErrors...)
	}
	if err := removeLocalStateFiles(cfg.StateDir); err != nil {
		return err
	}
	log.Printf("unregistered Node %s and removed maclet state from %s", state.NodeName, cfg.StateDir)
	return nil
}

func leaveCommand(args []string) error {
	flags := flag.NewFlagSet("leave", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cfg := leaveConfig{}
	flags.StringVar(&cfg.StateDir, "state-dir", defaultStatePath(), "maclet state directory")
	flags.StringVar(&cfg.NodeName, "node-name", "", "maclet Node to remove (defaults to the persisted state)")
	flags.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "kubeconfig with permission to remove the Node, Lease, and node-password Secret")
	flags.StringVar(&cfg.Context, "context", "", "kubeconfig context for removal")
	flags.BoolVar(&cfg.InsecureTLS, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	leaveContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLeave(leaveContext, cfg)
}
