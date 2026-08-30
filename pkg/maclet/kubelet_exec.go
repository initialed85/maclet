package maclet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	spdyserver "k8s.io/apimachinery/pkg/util/httpstream/spdy"
)

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
