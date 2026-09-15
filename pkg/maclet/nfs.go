package maclet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"
)

const genericNFSCSIDriver = "nfs.csi.k8s.io"

type genericNFSVolume struct {
	ClaimName    string
	PVName       string
	Server       string
	Share        string
	Subdir       string
	MountOptions []string
	ReadOnly     bool
}

func resolveGenericNFSVolume(ctx context.Context, client *APIClient, namespace string, volume Volume) (genericNFSVolume, error) {
	claimSource := volume.PersistentVolumeClaim
	if claimSource == nil || claimSource.ClaimName == "" {
		return genericNFSVolume{}, errors.New("volume is not a named PersistentVolumeClaim")
	}
	if client == nil {
		return genericNFSVolume{}, missingPeerStorageClientError("NFS PVC/PV")
	}
	claimPath := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/persistentvolumeclaims/" + url.PathEscape(claimSource.ClaimName)
	claimBody, err := client.Get(ctx, claimPath)
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("get PVC %s/%s: %w", namespace, claimSource.ClaimName, authorizedPeerStorageError("PVC "+namespace+"/"+claimSource.ClaimName, err))
	}
	var claim PersistentVolumeClaim
	if err := json.Unmarshal(claimBody, &claim); err != nil {
		return genericNFSVolume{}, fmt.Errorf("decode PVC %s/%s: %w", namespace, claimSource.ClaimName, err)
	}
	if string(claim.Status.Phase) != "Bound" {
		return genericNFSVolume{}, fmt.Errorf("PVC %s/%s is not Bound (phase %q)", namespace, claimSource.ClaimName, claim.Status.Phase)
	}
	if claim.Spec.VolumeName == "" {
		return genericNFSVolume{}, fmt.Errorf("PVC %s/%s has no bound PV", namespace, claimSource.ClaimName)
	}
	pvPath := "/api/v1/persistentvolumes/" + url.PathEscape(claim.Spec.VolumeName)
	pvBody, err := client.Get(ctx, pvPath)
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("get PV %s: %w", claim.Spec.VolumeName, authorizedPeerStorageError("PV "+claim.Spec.VolumeName, err))
	}
	var pv PersistentVolume
	if err := json.Unmarshal(pvBody, &pv); err != nil {
		return genericNFSVolume{}, fmt.Errorf("decode PV %s: %w", claim.Spec.VolumeName, err)
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != claim.Name || pv.Spec.ClaimRef.Namespace != namespace {
		return genericNFSVolume{}, fmt.Errorf("PV %s is not bound to PVC %s/%s", pv.Name, namespace, claim.Name)
	}
	if claim.UID != "" && pv.Spec.ClaimRef.UID != "" && pv.Spec.ClaimRef.UID != claim.UID {
		return genericNFSVolume{}, fmt.Errorf("PV %s claim UID does not match PVC %s/%s", pv.Name, namespace, claim.Name)
	}
	if pv.Spec.NFS != nil {
		return genericNFSVolume{}, fmt.Errorf("PV %s uses a direct NFS source; only CSI driver %s is supported", pv.Name, genericNFSCSIDriver)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != genericNFSCSIDriver {
		driver := ""
		if pv.Spec.CSI != nil {
			driver = pv.Spec.CSI.Driver
		}
		return genericNFSVolume{}, fmt.Errorf("PV %s uses unsupported CSI driver %q; want %s", pv.Name, driver, genericNFSCSIDriver)
	}
	server, err := validateNFSServer(pv.Spec.CSI.VolumeAttributes["server"])
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("PV %s server: %w", pv.Name, err)
	}
	share, err := validateNFSShare(pv.Spec.CSI.VolumeAttributes["share"])
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("PV %s share: %w", pv.Name, err)
	}
	subdir, err := validateNFSSubdir(pv.Spec.CSI.VolumeAttributes["subdir"])
	if err != nil {
		return genericNFSVolume{}, fmt.Errorf("PV %s subdir: %w", pv.Name, err)
	}
	if err := validateNFSMountOptions(pv.Spec.MountOptions); err != nil {
		return genericNFSVolume{}, fmt.Errorf("PV %s mount options: %w", pv.Name, err)
	}
	return genericNFSVolume{
		ClaimName:    claim.Name,
		PVName:       pv.Name,
		Server:       server,
		Share:        share,
		Subdir:       subdir,
		MountOptions: append([]string(nil), pv.Spec.MountOptions...),
		ReadOnly:     claimSource.ReadOnly,
	}, nil
}

func validateNFSServer(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("server is empty")
	}
	for _, character := range value {
		if unicode.IsSpace(character) || character == '/' || character == ',' || character == '\\' {
			return "", fmt.Errorf("server %q contains an unsafe character", value)
		}
	}
	return value, nil
}

func validateNFSShare(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("share %q must be an absolute path", value)
	}
	if hasParentPath(value) {
		return "", fmt.Errorf("share %q contains parent traversal", value)
	}
	return path.Clean(value), nil
}

func validateNFSSubdir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || hasParentPath(value) {
		return "", fmt.Errorf("subdir %q must remain beneath the export", value)
	}
	return path.Clean(value), nil
}

func hasParentPath(value string) bool {
	for _, component := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func validateNFSMountOptions(options []string) error {
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		option = strings.TrimSpace(option)
		if option == "" {
			return errors.New("empty mount option")
		}
		key, value, hasValue := strings.Cut(option, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if seen[key] {
			return fmt.Errorf("duplicate mount option %q", key)
		}
		seen[key] = true
		switch key {
		case "hard", "soft", "resvport", "locallocks", "nolock", "nolocks", "nolockd", "tcp", "udp":
			if hasValue {
				return fmt.Errorf("mount option %q does not take a value", key)
			}
		case "vers", "nfsvers":
			if !hasValue || (value != "3" && value != "4") {
				return fmt.Errorf("mount option %q must be 3 or 4", key)
			}
		case "port", "mountport", "rsize", "wsize", "timeo", "retrans":
			if !hasValue {
				return fmt.Errorf("mount option %q requires a numeric value", key)
			}
			number, err := strconv.Atoi(value)
			if err != nil || number <= 0 || number > 1<<31-1 {
				return fmt.Errorf("mount option %q has invalid numeric value", key)
			}
		default:
			return fmt.Errorf("mount option %q is not allowlisted", key)
		}
	}
	return nil
}

func isNFSNotFound(err error) bool {
	var apiErr *HTTPError
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}
