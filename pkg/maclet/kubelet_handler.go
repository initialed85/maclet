package maclet

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
)

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
