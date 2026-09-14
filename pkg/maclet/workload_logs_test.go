package maclet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveWorkloadLogsBeforeCleanupAndRecoverJournal(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "macker")
	script := `#!/bin/sh
case "$1" in
logs) printf 'terminated output\n' ;;
stop|rm) exit 0 ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(directory, "state")
	manager := newWorkloadManagerWithState(nil, binary, "192.0.2.10", stateDir)
	workload := &managedWorkload{UID: "uid-1", Namespace: "demo", Name: "native", PodContainerName: "app", ContainerName: "macker-demo-native", IP: "10.42.8.3"}
	manager.workloads[workload.UID] = workload
	if err := manager.removeWorkload(workload); err != nil {
		t.Fatal(err)
	}
	manager.retainWorkloadLocked(workload)
	if workload.LogFile == "" || workload.LogExpiresAt.IsZero() {
		t.Fatalf("archived workload = %#v", workload)
	}
	if err := manager.persistJournal(); err != nil {
		t.Fatal(err)
	}
	loaded := newWorkloadManagerWithState(nil, binary, "192.0.2.10", stateDir)
	if err := loaded.loadJournal(); err != nil {
		t.Fatal(err)
	}
	retained := loaded.retained["uid-1"]
	if retained == nil || retained.LogFile == "" {
		t.Fatalf("loaded retained workload = %#v", retained)
	}
	body, err := loaded.readRetainedLogs(retained)
	if err != nil || string(body) != "terminated output\n" {
		t.Fatalf("retained logs = %q, err=%v", body, err)
	}
}

func TestPruneRetainedLogsByTTL(t *testing.T) {
	stateDir := t.TempDir()
	manager := newWorkloadManagerWithState(nil, "", "", stateDir)
	if err := os.MkdirAll(manager.logsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.logsRoot, "expired.log"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.retained["uid-expired"] = &managedWorkload{
		UID: "uid-expired", Namespace: "demo", Name: "old", ContainerName: "container", Retained: true,
		LogFile: "expired.log", LogExpiresAt: time.Now().Add(-time.Minute),
	}
	if !manager.pruneRetainedLogsLocked(time.Now()) {
		t.Fatal("pruneRetainedLogsLocked() reported no change")
	}
	if _, err := os.Stat(filepath.Join(manager.logsRoot, "expired.log")); !os.IsNotExist(err) {
		t.Fatalf("expired log stat error = %v", err)
	}
	if len(manager.retained) != 0 {
		t.Fatalf("retained records = %#v", manager.retained)
	}
}

func TestRedactedLogsDoNotExposeEnvironment(t *testing.T) {
	if strings.Contains(redactMackerArgs([]string{"--env", "TOKEN=secret"}), "secret") {
		t.Fatal("redacted log invocation exposed environment")
	}
}
