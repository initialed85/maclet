package maclet

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type nfsMountSpec struct {
	Server       string
	Share        string
	MountOptions []string
	ReadOnly     bool
}

func nfsMountCommandArgs(spec nfsMountSpec, target string) ([]string, error) {
	server, err := validateNFSServer(spec.Server)
	if err != nil {
		return nil, err
	}
	share, err := validateNFSShare(spec.Share)
	if err != nil {
		return nil, err
	}
	if target == "" || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return nil, fmt.Errorf("mount target %q must be a clean absolute path", target)
	}
	if err := validateNFSMountOptions(spec.MountOptions); err != nil {
		return nil, err
	}
	options := append([]string(nil), spec.MountOptions...)
	if spec.ReadOnly {
		options = append(options, "ro")
	}
	args := []string{"mount_nfs"}
	if len(options) != 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	args = append(args, server+":"+share, target)
	return args, nil
}

func runNFSMount(ctx context.Context, useSudo bool, spec nfsMountSpec, target string) error {
	args, err := nfsMountCommandArgs(spec, target)
	if err != nil {
		return err
	}
	command := privilegedCommand(useSudo, args[0], args[1:]...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount NFS %s:%s on %s: %w: %s", spec.Server, spec.Share, target, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runNFSUnmount(ctx context.Context, useSudo bool, target string) error {
	if target == "" || !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("unmount target %q must be a clean absolute path", target)
	}
	command := privilegedCommand(useSudo, "umount", target)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount NFS %s: %w: %s", target, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func nfsMountHelperCommand(args []string) error {
	flags := flag.NewFlagSet("nfs-mount", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	server := flags.String("server", "", "NFS server")
	share := flags.String("share", "", "NFS export")
	target := flags.String("target", "", "absolute mount target")
	readOnly := flags.Bool("read-only", false, "mount read-only")
	var options stringSliceFlag
	flags.Var(&options, "option", "NFS mount option (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runNFSMount(context.Background(), false, nfsMountSpec{Server: *server, Share: *share, MountOptions: options, ReadOnly: *readOnly}, *target)
}

func nfsUnmountHelperCommand(args []string) error {
	flags := flag.NewFlagSet("nfs-unmount", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	target := flags.String("target", "", "absolute mount target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return runNFSUnmount(context.Background(), false, *target)
}

func isNFSUnmountMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not mounted")
}

func nfsMountExecutable() (string, error) {
	path, err := exec.LookPath("mount_nfs")
	if err != nil {
		return "", fmt.Errorf("mount_nfs is unavailable: %w", err)
	}
	return path, nil
}
