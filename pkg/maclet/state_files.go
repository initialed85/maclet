package maclet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func mustReadFile(path string) []byte {
	body, _ := os.ReadFile(path)
	return body
}

func invokingOwner() (int, int, bool, error) {
	if os.Geteuid() != 0 || os.Getenv("SUDO_UID") == "" || os.Getenv("SUDO_GID") == "" {
		return 0, 0, false, nil
	}
	uid, err := strconv.Atoi(os.Getenv("SUDO_UID"))
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse SUDO_UID: %w", err)
	}
	gid, err := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err != nil {
		return 0, 0, false, fmt.Errorf("parse SUDO_GID: %w", err)
	}
	return uid, gid, true, nil
}

func chownToInvokingUser(path string) error {
	uid, gid, needed, err := invokingOwner()
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("set owner on %s: %w", path, err)
	}
	return nil
}

func writePrivateFile(path string, body []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".maclet-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	if err := chownToInvokingUser(path); err != nil {
		return err
	}
	return nil
}
