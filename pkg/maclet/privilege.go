package maclet

import (
	"errors"
	"os"
	"os/exec"
)

func privilegedCommand(useSudo bool, name string, args ...string) *exec.Cmd {
	if useSudo {
		args = append([]string{"-n", name}, args...)
		return exec.Command("sudo", args...)
	}
	return exec.Command(name, args...)
}

func preparePrivileges(cfg *JoinConfig) error {
	if cfg.VXLANBinary == "" || os.Geteuid() == 0 {
		return nil
	}
	command := exec.Command("sudo", "-n", "true")
	if err := command.Run(); err != nil {
		return errors.New("VXLAN and Darwin route/ARP setup require root or passwordless sudo; rerun maclet join via sudo")
	}
	cfg.useSudo = true
	return nil
}
