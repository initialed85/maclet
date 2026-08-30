package main

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
	defaultCleanupNamespace  = "maclet-system"
	defaultCleanupInterval   = 15 * time.Second
	defaultCleanupStaleAfter = 45 * time.Second
)

type cleanupControllerConfig struct {
	NodeName    string
	Namespace   string
	Kubeconfig  string
	Context     string
	Interval    time.Duration
	StaleAfter  time.Duration
	InsecureTLS bool
}

func listAssignedPodsInNamespace(ctx context.Context, client *APIClient, nodeName, namespace string) ([]Pod, error) {
	query := url.Values{
		"fieldSelector": []string{"spec.nodeName=" + nodeName},
		"labelSelector": []string{nativeWorkloadLabelKey + "=" + nativeWorkloadLabelValue},
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?" + query.Encode()
	body, err := client.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var pods PodList
	if err := json.Unmarshal(body, &pods); err != nil {
		return nil, fmt.Errorf("decode cleanup PodList: %w", err)
	}
	return pods.Items, nil
}

func cleanupTerminatingPods(ctx context.Context, client *APIClient, nodeName, namespace string, staleAfter time.Duration, now time.Time) (int, error) {
	pods, err := listAssignedPodsInNamespace(ctx, client, nodeName, namespace)
	if err != nil {
		return 0, fmt.Errorf("list native Pods for cleanup: %w", err)
	}
	removed := 0
	var cleanupErrors []error
	for _, pod := range pods {
		if pod.Metadata.DeletionTimestamp == "" {
			continue
		}
		deletedAt, err := time.Parse(time.RFC3339Nano, pod.Metadata.DeletionTimestamp)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("parse deletion timestamp for Pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			continue
		}
		if staleAfter > 0 && now.Sub(deletedAt) < staleAfter {
			continue
		}
		path := podPath(pod) + "?gracePeriodSeconds=0"
		if _, err := client.delete(ctx, path); err != nil {
			var apiErr *HTTPError
			if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
				continue
			}
			cleanupErrors = append(cleanupErrors, fmt.Errorf("force-delete native Pod %s/%s: %w", pod.Metadata.Namespace, pod.Metadata.Name, err))
			continue
		}
		removed++
		log.Printf("force-deleted stale native Pod %s/%s assigned to %s", pod.Metadata.Namespace, pod.Metadata.Name, nodeName)
	}
	return removed, errors.Join(cleanupErrors...)
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
	cleanup := func() {
		removed, cleanupErr := cleanupTerminatingPods(ctx, client, cfg.NodeName, cfg.Namespace, cfg.StaleAfter, time.Now())
		if cleanupErr != nil {
			log.Printf("warning: native Pod cleanup: %v", cleanupErr)
		}
		if removed > 0 {
			log.Printf("removed %d stale native Pod(s)", removed)
		}
	}
	cleanup()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			cleanup()
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
