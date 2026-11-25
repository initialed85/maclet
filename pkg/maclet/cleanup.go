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
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultCleanupNamespace           = "maclet-system"
	defaultCleanupInterval            = 15 * time.Second
	defaultCleanupStaleAfter          = 45 * time.Second
	defaultNativePodCleanupStaleAfter = 5 * time.Second
)

type cleanupControllerConfig struct {
	NodeName    string
	Namespace   string
	Kubeconfig  string
	Context     string
	Interval    time.Duration
	StaleAfter  time.Duration
	Once        bool
	InsecureTLS bool
}

func listAssignedPodsInNamespace(ctx context.Context, client *APIClient, nodeName, namespace string) ([]Pod, error) {
	// No label selector: the sweeper must see every Pod bound to this node, including
	// ones maclet declined to adopt, and NodeRestriction authorizes exactly that read
	// (`spec.nodeName=<this node>`) for a node credential.
	query := url.Values{
		"fieldSelector": []string{"spec.nodeName=" + nodeName},
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?" + query.Encode()
	body, err := client.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, fmt.Errorf("decode cleanup PodList: %w", err)
	}
	return pods.Items, nil
}

func cleanupPodList(ctx context.Context, client *APIClient, pods []Pod, nodeName string, staleAfter time.Duration, now time.Time) (int, error) {
	removed := 0
	var cleanupErrors []error
	for _, pod := range pods {
		// Only the node and deletion checks, deliberately. The native label gates
		// *adoption*; it must not gate this, because a Pod bound to a maclet node can be
		// acknowledged by nothing else whether or not maclet would supervise it. Without
		// this, a Pod maclet refused (unlabelled, unsupported) that was deleted would sit
		// in Terminating forever, and a Deployment or StatefulSet that landed a Pod on a
		// darwin node by mistake would stall waiting for a replacement slot held by the
		// stuck object. There is no kubelet here to do it instead. The node check is also
		// kept client-side as defense-in-depth alongside the API field/label selectors.
		if pod.Spec.NodeName != nodeName || pod.ObjectMeta.DeletionTimestamp == nil {
			continue
		}
		deletedAt := pod.ObjectMeta.DeletionTimestamp.Time
		if staleAfter > 0 && now.Sub(deletedAt) < staleAfter {
			continue
		}
		path := podPath(pod) + "?gracePeriodSeconds=0"
		if _, err := client.Delete(ctx, path); err != nil {
			var apiErr *HTTPError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
				continue
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("force-delete Pod %s/%s assigned to %s: %w", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, nodeName, err))
			continue
		}
		removed++
		log.Printf("force-deleted stale Pod %s/%s assigned to %s past its grace period; no other agent can acknowledge a deletion on a darwin node", pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, nodeName)
	}
	return removed, errors.Join(cleanupErrors...)
}

func cleanupTerminatingPods(ctx context.Context, client *APIClient, nodeName, namespace string, staleAfter time.Duration, now time.Time) (int, error) {
	pods, err := listAssignedPodsInNamespace(ctx, client, nodeName, namespace)
	if err != nil {
		return 0, fmt.Errorf("list Pods assigned to %s for cleanup: %w", nodeName, err)
	}
	return cleanupPodList(ctx, client, pods, nodeName, staleAfter, now)
}

func cleanupTerminatingPodsClusterWide(ctx context.Context, client *APIClient, nodeName string, staleAfter time.Duration, now time.Time) (int, error) {
	query := url.Values{
		"fieldSelector": []string{"spec.nodeName=" + nodeName},
	}
	body, err := client.Get(ctx, "/api/v1/pods?"+query.Encode())
	if err != nil {
		return 0, fmt.Errorf("list Pods assigned to %s for cluster-wide cleanup: %w", nodeName, err)
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return 0, fmt.Errorf("decode cluster-wide cleanup PodList: %w", err)
	}
	return cleanupPodList(ctx, client, pods.Items, nodeName, staleAfter, now)
}

func runCleanupController(ctx context.Context, cfg cleanupControllerConfig) error {
	if cfg.NodeName == "" {
		return errors.New("--node-name is required")
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaultCleanupNamespace
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultCleanupInterval
	}
	if cfg.StaleAfter < 0 {
		return errors.New("--stale-after cannot be negative")
	}
	var client *APIClient
	var err error
	if cfg.Kubeconfig != "" {
		client, _, err = loadPeerAPIClient("", cfg.Kubeconfig, cfg.Context, cfg.InsecureTLS)
		if err != nil {
			return fmt.Errorf("load cleanup kubeconfig: %w", err)
		}
	} else {
		client, err = inClusterAPIClient()
		if err != nil {
			return fmt.Errorf("load in-cluster cleanup credentials: %w", err)
		}
	}
	cleanup := func() error {
		removed, cleanupErr := cleanupTerminatingPods(ctx, client, cfg.NodeName, cfg.Namespace, cfg.StaleAfter, time.Now())
		if removed > 0 {
			log.Printf("removed %d stale native Pod(s)", removed)
		}
		return cleanupErr
	}
	if err := cleanup(); err != nil {
		if cfg.Once {
			return err
		}
		log.Printf("warning: native Pod cleanup: %v", err)
	}
	if cfg.Once {
		return nil
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := cleanup(); err != nil {
				log.Printf("warning: native Pod cleanup: %v", err)
			}
		}
	}
}

func cleanupControllerCommand(args []string) error {
	flags := flag.NewFlagSet("cleanup-controller", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cfg := cleanupControllerConfig{}
	flags.StringVar(&cfg.NodeName, "node-name", "", "maclet Node whose stale native Pods should be removed")
	flags.StringVar(&cfg.Namespace, "namespace", defaultCleanupNamespace, "namespace containing native workloads")
	flags.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "privileged cleanup kubeconfig (defaults to in-cluster credentials)")
	flags.StringVar(&cfg.Context, "context", "", "kubeconfig context for cleanup")
	flags.DurationVar(&cfg.Interval, "interval", defaultCleanupInterval, "cleanup polling interval")
	flags.DurationVar(&cfg.StaleAfter, "stale-after", defaultCleanupStaleAfter, "minimum age of a Pod deletion timestamp before force deletion")
	flags.BoolVar(&cfg.Once, "once", false, "run one cleanup pass and exit")
	flags.BoolVar(&cfg.InsecureTLS, "insecure-skip-tls-verify", false, "disable TLS verification (development only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runCleanupController(ctx, cfg)
}
