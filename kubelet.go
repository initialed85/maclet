package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rancher/remotedialer"
	"k8s.io/apimachinery/pkg/util/httpstream"
	spdyserver "k8s.io/apimachinery/pkg/util/httpstream/spdy"
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
	body, err := client.get(ctx, "/v1-k3s/apiservers")
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
		Handler:           &kubeletHandler{manager: manager},
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

type kubeletHandler struct {
	manager *workloadManager
}

func (h *kubeletHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.TLS != nil && len(request.TLS.PeerCertificates) == 0 {
		http.Error(response, "kubelet client certificate required", http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasPrefix(request.URL.Path, kubeletContainerLogsPrefix):
		h.serveLogs(response, request)
	case strings.HasPrefix(request.URL.Path, kubeletExecPrefix):
		h.serveExec(response, request)
	default:
		http.NotFound(response, request)
	}
}

func kubeletPathParts(path, prefix string) (namespace, podName, containerName string, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	values := make([]string, len(parts))
	for index, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil || value == "" {
			return "", "", "", false
		}
		values[index] = value
	}
	return values[0], values[1], values[2], true
}

func (h *kubeletHandler) findWorkload(response http.ResponseWriter, request *http.Request, prefix string) (*managedWorkload, bool) {
	namespace, podName, containerName, ok := kubeletPathParts(request.URL.Path, prefix)
	if !ok {
		http.Error(response, "invalid kubelet resource path", http.StatusBadRequest)
		return nil, false
	}
	workload, err := h.manager.findContainer(namespace, podName, containerName)
	if errors.Is(err, errNotFound) {
		http.Error(response, "container is not managed by maclet", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		http.Error(response, err.Error(), http.StatusNotFound)
		return nil, false
	}
	return workload, true
}

func (h *kubeletHandler) serveLogs(response http.ResponseWriter, request *http.Request) {
	workload, ok := h.findWorkload(response, request, kubeletContainerLogsPrefix)
	if !ok {
		return
	}
	if status, found, err := h.manager.containerStatus(workload.ContainerName); err != nil {
		http.Error(response, fmt.Sprintf("inspect container: %v", err), http.StatusInternalServerError)
		return
	} else if !found {
		http.Error(response, "container is not present", http.StatusNotFound)
		return
	} else if status == "" {
		http.Error(response, "container has no status", http.StatusNotFound)
		return
	}
	args := []string{"logs"}
	if request.URL.Query().Get("follow") == "true" {
		args = append(args, "--follow")
	}
	args = append(args, workload.ContainerName)
	command, err := h.manager.mackerCommand(args...)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	command = exec.CommandContext(request.Context(), command.Path, command.Args[1:]...)
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	command.Stdout = &kubeletFlushWriter{response: response}
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		// The HTTP status and any available output have already been sent. A
		// failed follow is therefore only useful in the daemon log.
		log.Printf("warning: stream logs for %s/%s: %v", workload.Namespace, workload.Name, err)
	}
}

type kubeletFlushWriter struct {
	response http.ResponseWriter
}

func (w *kubeletFlushWriter) Write(body []byte) (int, error) {
	written, err := w.response.Write(body)
	if flusher, ok := w.response.(http.Flusher); ok {
		flusher.Flush()
	}
	return written, err
}

type kubeletStream struct {
	stream    httpstream.Stream
	replySent <-chan struct{}
}

func (h *kubeletHandler) serveExec(response http.ResponseWriter, request *http.Request) {
	workload, ok := h.findWorkload(response, request, kubeletExecPrefix)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		http.Error(response, "exec requires POST", http.StatusMethodNotAllowed)
		return
	}
	status, found, err := h.manager.containerStatus(workload.ContainerName)
	if err != nil {
		http.Error(response, fmt.Sprintf("inspect container: %v", err), http.StatusInternalServerError)
		return
	}
	if !found || status != "running" {
		http.Error(response, "container is not running", http.StatusNotFound)
		return
	}

	_, err = httpstream.Handshake(request, response, []string{
		"v4.channel.k8s.io",
		"v3.channel.k8s.io",
		"v2.channel.k8s.io",
		"channel.k8s.io",
	})
	if err != nil {
		return
	}
	streamCh := make(chan kubeletStream, 8)
	upgrader := spdyserver.NewResponseUpgrader()
	connection := upgrader.UpgradeResponse(response, request, func(stream httpstream.Stream, replySent <-chan struct{}) error {
		select {
		case streamCh <- kubeletStream{stream: stream, replySent: replySent}:
			return nil
		case <-request.Context().Done():
			return request.Context().Err()
		}
	})
	if connection == nil {
		return
	}
	defer connection.Close()

	query := request.URL.Query()
	wantStdin := kubeletQueryBool(query, "stdin", "input")
	wantStdout := kubeletQueryBool(query, "stdout", "output")
	wantTty := kubeletQueryBool(query, "tty")
	wantStderr := kubeletQueryBool(query, "stderr", "error") && !wantTty
	wantResize := wantTty
	expected := 1
	for _, wanted := range []bool{wantStdin, wantStdout, wantStderr, wantResize} {
		if wanted {
			expected++
		}
	}
	streams := make(map[string]httpstream.Stream, expected)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for len(streams) < expected {
		select {
		case incoming := <-streamCh:
			streamType := incoming.stream.Headers().Get(kubeletStreamTypeHeader)
			if _, duplicate := streams[streamType]; duplicate {
				_ = incoming.stream.Reset()
				continue
			}
			switch streamType {
			case kubeletStreamError, kubeletStreamStdin, kubeletStreamStdout, kubeletStreamStderr, kubeletStreamResize:
				streams[streamType] = incoming.stream
			default:
				_ = incoming.stream.Reset()
			}
		case <-deadline.C:
			return
		case <-request.Context().Done():
			return
		}
	}
	if _, exists := streams[kubeletStreamError]; !exists {
		return
	}
	if resize := streams[kubeletStreamResize]; resize != nil {
		go func() { _, _ = io.Copy(io.Discard, resize) }()
	}
	commandArgs := query["command"]
	if len(commandArgs) == 0 {
		commandArgs = []string{"/bin/sh"}
	}
	mackerArgs := []string{"exec"}
	if wantStdin {
		mackerArgs = append(mackerArgs, "--interactive")
	}
	if wantTty {
		mackerArgs = append(mackerArgs, "--tty")
	}
	mackerArgs = append(mackerArgs, workload.ContainerName, "--")
	mackerArgs = append(mackerArgs, commandArgs...)
	command, err := h.manager.mackerCommand(mackerArgs...)
	if err != nil {
		writeKubeletExecStatus(streams[kubeletStreamError], fmt.Errorf("start Macker exec: %w", err))
		return
	}
	command = exec.CommandContext(request.Context(), command.Path, command.Args[1:]...)
	if wantStdin {
		command.Stdin = streams[kubeletStreamStdin]
	}
	stdout := streams[kubeletStreamStdout]
	stderr := streams[kubeletStreamStderr]
	var commandError bytes.Buffer
	if wantTty {
		if stdout == nil {
			command.Stdout = io.Discard
			command.Stderr = &commandError
		} else {
			command.Stdout = io.MultiWriter(stdout, &commandError)
			command.Stderr = io.MultiWriter(stdout, &commandError)
		}
	} else {
		if stdout == nil {
			command.Stdout = io.Discard
		} else {
			command.Stdout = stdout
		}
		if stderr == nil {
			command.Stderr = &commandError
		} else {
			command.Stderr = io.MultiWriter(stderr, &commandError)
		}
	}
	runErr := command.Run()
	statusErr := runErr
	if runErr != nil && commandError.Len() > 0 {
		statusErr = fmt.Errorf("%w: %s", runErr, strings.TrimSpace(commandError.String()))
	}
	writeKubeletExecStatus(streams[kubeletStreamError], statusErr)
	for _, streamType := range []string{kubeletStreamError, kubeletStreamStdin, kubeletStreamStdout, kubeletStreamStderr, kubeletStreamResize} {
		if stream := streams[streamType]; stream != nil {
			_ = stream.Close()
		}
	}
}

func kubeletQueryBool(query url.Values, names ...string) bool {
	for _, name := range names {
		value := query.Get(name)
		if value == "true" || value == "1" {
			return true
		}
	}
	return false
}

func kubeletExecExitCode(runErr error) (int, bool) {
	if runErr == nil {
		return 0, false
	}
	// Macker reports the native child's status in its wrapped error, then its
	// own CLI exits non-zero. Prefer that inner status over the CLI's generic 1.
	message := runErr.Error()
	if marker := strings.LastIndex(message, "exit status "); marker >= 0 {
		value := strings.TrimSpace(message[marker+len("exit status "):])
		if code, err := strconv.Atoi(value); err == nil && code >= 0 {
			return code, true
		}
	}
	var exitError *exec.ExitError
	if errors.As(runErr, &exitError) && exitError.ExitCode() >= 0 {
		return exitError.ExitCode(), true
	}
	return 0, false
}

func writeKubeletExecStatus(stream io.Writer, runErr error) {
	status := map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     "Success",
	}
	if runErr != nil {
		status["status"] = "Failure"
		status["message"] = runErr.Error()
		if code, ok := kubeletExecExitCode(runErr); ok {
			status["reason"] = "NonZeroExitCode"
			status["details"] = map[string]any{
				"causes": []map[string]string{{"reason": "ExitCode", "message": strconv.Itoa(code)}},
			}
		}
	}
	body, err := json.Marshal(status)
	if err == nil {
		_, _ = stream.Write(body)
	}
}
