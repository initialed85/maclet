package maclet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (m *workloadManager) archiveWorkloadLogs(workload *managedWorkload) error {
	if workload == nil || workload.ContainerName == "" || m.logsRoot == "" {
		return nil
	}
	output, err := m.mackerOutput("logs", workload.ContainerName)
	if err != nil {
		if strings.Contains(err.Error(), "was not found") || strings.Contains(err.Error(), "not a detached workload") {
			return nil
		}
		return err
	}
	if len(output) > maxRetainedNativeLogBytes {
		output = output[len(output)-maxRetainedNativeLogBytes:]
	}
	digest := sha256.Sum256([]byte(workload.Namespace + "\x00" + workload.UID + "\x00" + workload.PodContainerName))
	fileName := hex.EncodeToString(digest[:]) + ".log"
	if err := os.MkdirAll(m.logsRoot, 0o700); err != nil {
		return fmt.Errorf("create retained log directory: %w", err)
	}
	if err := writePrivateFile(filepath.Join(m.logsRoot, fileName), output, 0o600); err != nil {
		return fmt.Errorf("write retained logs: %w", err)
	}
	workload.LogFile = fileName
	retention := m.logTTL
	if retention <= 0 {
		retention = defaultNativeLogRetention
	}
	workload.LogExpiresAt = time.Now().Add(retention)
	return nil
}

func (m *workloadManager) retainWorkloadLocked(workload *managedWorkload) {
	if workload == nil {
		return
	}
	workload.Retained = true
	workload.IP = ""
	workload.VolumePaths = nil
	workload.NFSMounts = nil
	if workload.LogExpiresAt.IsZero() {
		retention := m.logTTL
		if retention <= 0 {
			retention = defaultNativeLogRetention
		}
		workload.LogExpiresAt = time.Now().Add(retention)
	}
	m.retained[workload.UID] = workload
	delete(m.workloads, workload.UID)
}

func (m *workloadManager) pruneRetainedLogsLocked(now time.Time) bool {
	changed := false
	for uid, workload := range m.retained {
		if workload.LogExpiresAt.IsZero() || now.Before(workload.LogExpiresAt) {
			continue
		}
		if workload.LogFile != "" && m.logsRoot != "" {
			_ = os.Remove(filepath.Join(m.logsRoot, filepath.Base(workload.LogFile)))
		}
		delete(m.retained, uid)
		changed = true
	}
	return changed
}

func (m *workloadManager) retainedLogPath(workload *managedWorkload) string {
	if workload == nil || workload.LogFile == "" || m.logsRoot == "" {
		return ""
	}
	return filepath.Join(m.logsRoot, filepath.Base(workload.LogFile))
}

func (m *workloadManager) readRetainedLogs(workload *managedWorkload) ([]byte, error) {
	path := m.retainedLogPath(workload)
	if path == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

func isMissingRetainedLog(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
