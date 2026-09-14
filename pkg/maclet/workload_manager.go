package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nativeWorkloadLabelKey             = "k8s-darwin.dev/native"
	nativeWorkloadLabelValue           = "true"
	nativeDisablePortForwardAnnotation = "k8s-darwin.dev/disable-port-forward"
	workloadRetryDelay                 = 10 * time.Second
	defaultNativeLogRetention          = 24 * time.Hour
	maxRetainedNativeLogBytes          = 1 << 20
)

type nfsMountRecord struct {
	Target string       `json:"target"`
	Spec   nfsMountSpec `json:"spec"`
}

type managedWorkload struct {
	UID              string
	Namespace        string
	Name             string
	PodContainerName string
	ContainerName    string
	IP               string
	RestartCount     int32
	VolumePaths      []string
	HostNetwork      bool
	Retained         bool
	LogFile          string
	LogExpiresAt     time.Time
	NFSMounts        []nfsMountRecord
	RetryAfter       time.Time
}

type mackerInspection struct {
	Status            string     `json:"status"`
	PID               int        `json:"pid"`
	WorkloadPID       int        `json:"workload_pid"`
	ExitCode          *int32     `json:"exit_code"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	TerminationSignal string     `json:"termination_signal"`
	TerminationReason string     `json:"termination_reason"`
}

type workloadJournalRecord struct {
	UID              string           `json:"uid"`
	Namespace        string           `json:"namespace,omitempty"`
	Name             string           `json:"name,omitempty"`
	ContainerName    string           `json:"containerName"`
	PodContainerName string           `json:"podContainerName,omitempty"`
	IP               string           `json:"ip,omitempty"`
	RestartCount     int32            `json:"restartCount,omitempty"`
	VolumePaths      []string         `json:"volumePaths,omitempty"`
	HostNetwork      bool             `json:"hostNetwork,omitempty"`
	Retained         bool             `json:"retained,omitempty"`
	LogFile          string           `json:"logFile,omitempty"`
	LogExpiresAt     string           `json:"logExpiresAt,omitempty"`
	NFSMounts        []nfsMountRecord `json:"nfsMounts,omitempty"`
}

type workloadJournal struct {
	Version   int                     `json:"version"`
	Workloads []workloadJournalRecord `json:"workloads"`
}

type workloadManager struct {
	network      *DarwinNetworkHandle
	mackerBinary string
	nodeIP       string
	apiClient    *APIClient
	journalPath  string
	volumeRoot   string
	workloads    map[string]*managedWorkload
	retained     map[string]*managedWorkload
	logsRoot     string
	nfsRoot      string
	logTTL       time.Duration
	useSudo      bool
	mountNFS     func(context.Context, bool, nfsMountSpec, string) error
	unmountNFS   func(bool, string) error
	isMountpoint func(string, nfsMountSpec) (bool, error)
	debug        bool
	mu           sync.RWMutex
}

func newWorkloadManager(network *DarwinNetworkHandle, mackerBinary, nodeIP string) *workloadManager {
	return newWorkloadManagerWithState(network, mackerBinary, nodeIP, "")
}

func newWorkloadManagerWithState(network *DarwinNetworkHandle, mackerBinary, nodeIP, stateDir string) *workloadManager {
	journalPath := ""
	if stateDir != "" {
		journalPath = filepath.Join(stateDir, "workloads.json")
	}
	return &workloadManager{
		network:      network,
		mackerBinary: mackerBinary,
		nodeIP:       nodeIP,
		journalPath:  journalPath,
		workloads:    make(map[string]*managedWorkload),
		retained:     make(map[string]*managedWorkload),
		volumeRoot:   filepath.Join(stateDir, "volumes"),
		logsRoot:     filepath.Join(stateDir, "logs"),
		nfsRoot:      filepath.Join(stateDir, "nfs"),
		logTTL:       defaultNativeLogRetention,
	}
}

func (m *workloadManager) loadJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadJournalLocked()
}

func (m *workloadManager) loadJournalLocked() error {
	if m.journalPath == "" {
		return nil
	}
	body, err := os.ReadFile(m.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workload journal: %w", err)
	}
	var journal workloadJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return fmt.Errorf("decode workload journal: %w", err)
	}
	if journal.Version != 1 {
		return fmt.Errorf("unsupported workload journal version %d", journal.Version)
	}
	for _, record := range journal.Workloads {
		if record.UID == "" || record.ContainerName == "" {
			return errors.New("workload journal contains an incomplete record")
		}
		workload := &managedWorkload{
			UID:              record.UID,
			Namespace:        record.Namespace,
			Name:             record.Name,
			PodContainerName: record.PodContainerName,
			ContainerName:    record.ContainerName,
			IP:               record.IP,
			RestartCount:     record.RestartCount,
			VolumePaths:      append([]string(nil), record.VolumePaths...),
			HostNetwork:      record.HostNetwork,
			Retained:         record.Retained,
			LogFile:          record.LogFile,
			NFSMounts:        append([]nfsMountRecord(nil), record.NFSMounts...),
		}
		if record.LogExpiresAt != "" {
			if parsed, parseErr := time.Parse(time.RFC3339Nano, record.LogExpiresAt); parseErr == nil {
				workload.LogExpiresAt = parsed
			}
		}
		if workload.Retained {
			m.retained[record.UID] = workload
		} else {
			m.workloads[record.UID] = workload
		}
	}
	if m.pruneRetainedLogsLocked(time.Now()) {
		return m.persistJournalLocked()
	}
	return nil
}

func (m *workloadManager) persistJournal() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persistJournalLocked()
}

func (m *workloadManager) persistJournalLocked() error {
	if m.journalPath == "" {
		return nil
	}
	records := make([]workloadJournalRecord, 0, len(m.workloads)+len(m.retained))
	appendRecord := func(workload *managedWorkload) {
		record := workloadJournalRecord{
			UID: workload.UID, Namespace: workload.Namespace, Name: workload.Name,
			PodContainerName: workload.PodContainerName, ContainerName: workload.ContainerName,
			IP: workload.IP, RestartCount: workload.RestartCount,
			VolumePaths: append([]string(nil), workload.VolumePaths...),
			HostNetwork: workload.HostNetwork, Retained: workload.Retained,
			LogFile:   workload.LogFile,
			NFSMounts: append([]nfsMountRecord(nil), workload.NFSMounts...),
		}
		if !workload.LogExpiresAt.IsZero() {
			record.LogExpiresAt = workload.LogExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		records = append(records, record)
	}
	for _, workload := range m.workloads {
		appendRecord(workload)
	}
	for _, workload := range m.retained {
		appendRecord(workload)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].UID < records[j].UID })
	body, err := json.MarshalIndent(workloadJournal{Version: 1, Workloads: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workload journal: %w", err)
	}
	if err := writePrivateFile(m.journalPath, append(body, '\n'), 0600); err != nil {
		return fmt.Errorf("write workload journal: %w", err)
	}
	return nil
}

func formatCommandArgs(args []string) string {
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func (m *workloadManager) mackerCommand(args ...string) (*exec.Cmd, error) {
	binary := m.mackerBinary
	if binary == "" {
		var err error
		binary, err = exec.LookPath("macker")
		if err != nil {
			return nil, errors.New("macker executable was not found; set --macker-binary or add macker to PATH")
		}
	}
	return exec.Command(binary, args...), nil
}

func (m *workloadManager) mackerOutput(args ...string) ([]byte, error) {
	command, err := m.mackerCommand(args...)
	if err != nil {
		return nil, err
	}
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("macker %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("macker %s: %w (%s)", strings.Join(args, " "), err, message)
	}
	return output, nil
}

func mackerImageMissing(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "image is not present in local storage") ||
		strings.Contains(message, "pull or build it first")
}

// startMackerWorkload starts a native workload and lazily pulls its Darwin
// image when the local Macker store does not contain it. Macker deliberately
// keeps pulling out of its run command, so a fresh maclet node needs this
// small orchestration layer to make an assigned Pod self-sufficient.
func (m *workloadManager) startMackerWorkload(args []string, image string) error {
	if _, err := m.mackerOutput(args...); err == nil {
		return nil
	} else if !mackerImageMissing(err) {
		return err
	} else {
		if image == "" {
			return fmt.Errorf("Macker reported a missing image but the Pod image is empty: %w", err)
		}
		if m.debug {
			log.Printf("debug: Macker image %s is not local; pulling it before retrying workload", image)
		}
		if _, pullErr := m.mackerOutput("pull", image); pullErr != nil {
			return fmt.Errorf("pull Macker image %s: %w (initial run: %v)", image, pullErr, err)
		}
		if _, retryErr := m.mackerOutput(args...); retryErr != nil {
			return fmt.Errorf("retry Macker workload after pulling image %s: %w", image, retryErr)
		}
		return nil
	}
}

func mackerContainerStatus(output, name string) (status string, found bool) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 && fields[0] == name {
			return fields[2], true
		}
	}
	return "", false
}

func (m *workloadManager) containerStatus(name string) (string, bool, error) {
	output, err := m.mackerOutput("ps", "--all")
	if err != nil {
		return "", false, err
	}
	status, found := mackerContainerStatus(string(output), name)
	return status, found, nil
}

func (m *workloadManager) waitForMackerRunning(name string) error {
	// Macker's launcher can remain alive briefly after the native workload has
	// already exited. Prefer inspect's workload lifecycle status over `ps` so
	// startup failures (for example EADDRINUSE) are not reported as Ready.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inspection, inspectionAvailable, err := m.containerInspection(name)
		if err != nil {
			return err
		}
		if inspectionAvailable {
			if inspection.Status != "running" {
				message := fmt.Sprintf("Macker workload exited during startup (status=%s)", inspection.Status)
				if output, logsErr := m.mackerOutput("logs", name); logsErr == nil && strings.TrimSpace(string(output)) != "" {
					message += ": " + strings.TrimSpace(string(output))
				}
				return errors.New(message)
			}
			// A launcher can report running while its child is still in a
			// startup retry loop. Hold the readiness decision until the
			// startup window has elapsed, then inspect once more.
			if time.Now().After(deadline) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		status, found, err := m.containerStatus(name)
		if err != nil {
			return err
		}
		if found && status == "running" {
			return nil
		}
		if found && status != "running" {
			return fmt.Errorf("Macker workload exited during startup (status=%s)", status)
		}
		if time.Now().After(deadline) {
			return errors.New("Macker workload did not become running within 5 seconds")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *workloadManager) findContainer(namespace, podName, containerName string) (*managedWorkload, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, workload := range m.workloads {
		if workload.Namespace != namespace || workload.Name != podName {
			continue
		}
		if workload.PodContainerName != "" && workload.PodContainerName != containerName {
			return nil, fmt.Errorf("container %q is not managed by maclet", containerName)
		}
		copy := *workload
		return &copy, nil
	}
	for _, workload := range m.retained {
		if workload.Namespace != namespace || workload.Name != podName {
			continue
		}
		if workload.PodContainerName != "" && workload.PodContainerName != containerName {
			return nil, fmt.Errorf("container %q is not managed by maclet", containerName)
		}
		copy := *workload
		return &copy, nil
	}
	return nil, errNotFound
}

func (m *workloadManager) containerInspection(name string) (*mackerInspection, bool, error) {
	output, err := m.mackerOutput("inspect", "--format", "json", name)
	if err != nil {
		// Keep compatibility with Macker versions predating inspect. The
		// lifecycle reconciler can still use ps status, but cannot report an
		// exit code until the newer Macker binary is installed.
		return nil, false, nil
	}
	var inspection mackerInspection
	if err := json.Unmarshal(output, &inspection); err != nil {
		return nil, true, fmt.Errorf("decode Macker inspect output for %s: %w", name, err)
	}
	return &inspection, true, nil
}
