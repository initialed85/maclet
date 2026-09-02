package maclet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rancher/remotedialer"
)

const (
	kubeletContainerLogsPrefix = "/containerLogs/"
	kubeletExecPrefix          = "/exec/"
	kubeletStreamTypeHeader    = "streamType"
	kubeletStreamError         = "error"
	kubeletStreamStdin         = "stdin"
	kubeletStreamStdout        = "stdout"
	kubeletStreamStderr        = "stderr"
	kubeletStreamResize        = "resize"
)

type kubeletServer struct {
	server *http.Server
	ln     net.Listener
}

type kubeletTunnel struct {
	cancel context.CancelFunc
}

func discoverKubeletTunnelServers(ctx context.Context, client *APIClient, fallback string) []string {
	servers := make([]string, 0, 1)
	body, err := client.Get(ctx, "/v1-k3s/apiservers")
	if err == nil {
		var addresses []string
		if decodeErr := json.Unmarshal(body, &addresses); decodeErr == nil {
			for _, address := range addresses {
				if !strings.Contains(address, "://") {
					address = "https://" + address
				}
				if parsed, parseErr := normalizeServer(address); parseErr == nil {
					servers = append(servers, parsed.String())
				}
			}
		}
	}
	if len(servers) == 0 {
		if !strings.Contains(fallback, "://") {
			fallback = "https://" + fallback
		}
		if parsed, parseErr := normalizeServer(fallback); parseErr == nil {
			servers = append(servers, parsed.String())
		}
	}
	sort.Strings(servers)
	unique := servers[:0]
	for _, server := range servers {
		if len(unique) == 0 || unique[len(unique)-1] != server {
			unique = append(unique, server)
		}
	}
	return unique
}

func startKubeletTunnel(ctx context.Context, serverURL, nodeIP, certFile, keyFile, caFile string, port int) (*kubeletTunnel, error) {
	if !strings.Contains(serverURL, "://") {
		serverURL = "https://" + serverURL
	}
	server, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("parse kubelet tunnel server %q: %w", serverURL, err)
	}
	if server.Host == "" {
		return nil, fmt.Errorf("parse kubelet tunnel server %q: missing host", serverURL)
	}
	server.Scheme = "wss"
	server.Path = strings.TrimRight(server.Path, "/") + "/v1-k3s/connect"
	server.RawQuery = ""
	server.Fragment = ""
	clientCAPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read kubelet tunnel CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("kubelet tunnel CA is empty or invalid")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load kubelet tunnel client certificate: %w", err)
	}
	tunnelContext, cancel := context.WithCancel(ctx)
	connected := make(chan struct{})
	var connectedOnce sync.Once
	dialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      pool,
			Certificates: []tls.Certificate{certificate},
		},
	}
	authorize := func(protocol, address string) bool {
		host, requestedPort, splitErr := net.SplitHostPort(address)
		allowed := splitErr == nil && protocol == "tcp" && (host == "127.0.0.1" || host == "::1") && requestedPort == strconv.Itoa(port)
		if !allowed {
			log.Printf("warning: rejected kubelet remotedialer request proto=%s address=%s", protocol, address)
		}
		return allowed
	}
	localDialer := func(dialContext context.Context, network, address string) (net.Conn, error) {
		host, requestedPort, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, splitErr
		}
		if (host == "127.0.0.1" || host == "::1") && requestedPort == strconv.Itoa(port) {
			address = net.JoinHostPort(nodeIP, requestedPort)
		}
		return (&net.Dialer{}).DialContext(dialContext, network, address)
	}
	go func() {
		for tunnelContext.Err() == nil {
			err := remotedialer.ConnectToProxyWithDialer(tunnelContext, server.String(), nil, authorize, dialer, localDialer, func(context.Context, *remotedialer.Session) error {
				connectedOnce.Do(func() { close(connected) })
				log.Printf("connected kubelet remotedialer tunnel to %s", server.Host)
				return nil
			})
			if tunnelContext.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("warning: kubelet remotedialer tunnel %s: %v; reconnecting", server.Host, err)
			}
			select {
			case <-time.After(time.Second):
			case <-tunnelContext.Done():
				return
			}
		}
	}()
	select {
	case <-connected:
		return &kubeletTunnel{cancel: cancel}, nil
	case <-time.After(10 * time.Second):
		cancel()
		return nil, fmt.Errorf("kubelet remotedialer tunnel did not connect to %s", server.Host)
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func (t *kubeletTunnel) Close() {
	if t != nil && t.cancel != nil {
		t.cancel()
	}
}

type kubeletTunnelSupervisor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startKubeletTunnelSupervisor keeps the local kubelet available even when no
// API server accepts a remotedialer connection during startup. Each tunnel
// reconnects internally; the supervisor periodically refreshes the API-server
// list and adds newly discovered servers.
func startKubeletTunnelSupervisor(ctx context.Context, client *APIClient, fallback, nodeIP, certFile, keyFile, caFile string, port int) *kubeletTunnelSupervisor {
	return startKubeletTunnelSupervisorWithConnector(ctx, client, fallback, nodeIP, certFile, keyFile, caFile, port, 15*time.Second, startKubeletTunnel)
}

func startKubeletTunnelSupervisorWithConnector(ctx context.Context, client *APIClient, fallback, nodeIP, certFile, keyFile, caFile string, port int, interval time.Duration, connect func(context.Context, string, string, string, string, string, int) (*kubeletTunnel, error)) *kubeletTunnelSupervisor {
	tunnelContext, cancel := context.WithCancel(ctx)
	supervisor := &kubeletTunnelSupervisor{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(supervisor.done)
		tunnels := make(map[string]*kubeletTunnel)
		reconcile := func() {
			servers := discoverKubeletTunnelServers(tunnelContext, client, fallback)
			connected := 0
			for _, server := range servers {
				if _, found := tunnels[server]; found {
					connected++
					continue
				}
				tunnel, err := connect(tunnelContext, server, nodeIP, certFile, keyFile, caFile, port)
				if err != nil {
					log.Printf("warning: kubelet tunnel to %s unavailable: %v; will retry", server, err)
					continue
				}
				tunnels[server] = tunnel
				connected++
			}
			if connected == 0 {
				log.Printf("warning: no Kubernetes API server accepted a kubelet remotedialer tunnel; kubelet remains available and tunnel discovery will retry")
			}
		}
		reconcile()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-tunnelContext.Done():
				for _, tunnel := range tunnels {
					tunnel.Close()
				}
				return
			case <-ticker.C:
				reconcile()
			}
		}
	}()
	return supervisor
}

func (s *kubeletTunnelSupervisor) Close() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

func startKubeletServer(ctx context.Context, bindAddress string, manager *workloadManager, certFile, keyFile, clientCAFile string, port int) (*kubeletServer, error) {
	if manager == nil {
		return nil, errors.New("kubelet server requires a workload manager")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load kubelet serving certificate: %w", err)
	}
	clientCAPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read kubelet client CA: %w", err)
	}
	clientCAPool := x509.NewCertPool()
	if !clientCAPool.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("kubelet client CA is empty or invalid")
	}
	if bindAddress == "" {
		bindAddress = "0.0.0.0"
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("kubelet port %d is outside the valid range", port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindAddress, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen for kubelet HTTPS on %s: %w", net.JoinHostPort(bindAddress, strconv.Itoa(port)), err)
	}
	server := &http.Server{
		Handler:           &kubeletHandler{manager: manager, metrics: newResourceMetrics()},
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			// K3s performs HTTP-level client certificate authentication. Keep
			// the TLS handshake compatible with its unauthenticated probes,
			// while requiring a verified certificate for logs/exec below.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  clientCAPool,
		},
	}
	tlsListener := tls.NewListener(listener, server.TLSConfig)
	result := &kubeletServer{server: server, ln: tlsListener}
	go func() {
		if err := server.Serve(tlsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("warning: kubelet HTTPS server: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = result.Close()
	}()
	log.Printf("serving kubelet logs and exec on https://%s", net.JoinHostPort(bindAddress, strconv.Itoa(port)))
	return result, nil
}

func (s *kubeletServer) Close() error {
	if s == nil || s.server == nil {
		return nil
	}
	return s.server.Close()
}
