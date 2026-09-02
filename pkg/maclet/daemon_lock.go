package maclet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var errDaemonAlreadyRunning = errors.New("maclet is already running for this state directory")

type daemonLock struct {
	file *os.File
}

func acquireDaemonLock(stateDir string) (*daemonLock, error) {
	if stateDir == "" {
		return nil, errors.New("state directory is required for the maclet daemon lock")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory for daemon lock: %w", err)
	}
	if err := chownToInvokingUser(stateDir); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "daemon.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock %s: %w", path, err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("set daemon lock permissions: %w", err)
	}
	if err := chownToInvokingUser(path); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", errDaemonAlreadyRunning, stateDir)
		}
		return nil, fmt.Errorf("acquire daemon lock %s: %w", path, err)
	}
	return &daemonLock{file: file}, nil
}

func (lock *daemonLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
