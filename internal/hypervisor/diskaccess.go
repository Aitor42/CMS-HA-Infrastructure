package hypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

// qemuConfigPath is a variable so tests can point the parser at a fixture.
var qemuConfigPath = "/etc/libvirt/qemu.conf"

// CheckDiskAccess verifies the hypervisor can hand disk images to the qemu
// account without making them world-writable. For a system libvirt connection
// the supported model is libvirt dynamic ownership: qemu-img creates the file
// as the deploy user, libvirt chowns it to the qemu user when the domain
// starts, and the file stays mode 0600. If the operator disabled
// dynamic_ownership, fail before creating VMs instead of falling back to 0666.
func CheckDiskAccess(lv *libvirt.Client) error {
	if lv == nil || lv.URI != "qemu:///system" {
		// A session connection runs qemu as the invoking user, so 0600 is
		// enough. A remote URI is outside the local-storage model this lab
		// documents and is left to the operator.
		return nil
	}
	data, err := os.ReadFile(qemuConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", qemuConfigPath, err)
	}
	if !dynamicOwnershipEnabled(data) {
		return fmt.Errorf("libvirt dynamic_ownership is disabled in %s; enable it (dynamic_ownership = 1) so qemu can read mode-0600 disks without world-writable files", qemuConfigPath)
	}
	return nil
}

// EnsureStorageACL gives the qemu account traverse/read access to the storage
// directory and makes new files inherit a default ACL with no world access.
// This is what lets PXE disks created by virt-install be usable by qemu even
// when the deploy account cannot chmod a file after libvirt has chowned it.
// It is best-effort: RestrictDisk still fails closed if a file remains
// world-accessible and neither chmod nor sudo can fix it.
func EnsureStorageACL(storageDir string) error {
	if storageDir == "" {
		return nil
	}
	qemu := qemuUser()
	if qemu == "" {
		return nil
	}
	if _, err := exec.LookPath("setfacl"); err != nil {
		logging.Warn("setfacl is not installed; relying on libvirt ownership for disk permissions")
		return nil
	}
	commands := [][]string{
		{"-m", "u:" + qemu + ":rx", storageDir},
		{"-d", "-m", "u:" + qemu + ":rw,m::rw,o::---", storageDir},
	}
	for _, args := range commands {
		if out, err := exec.Command("setfacl", args...).CombinedOutput(); err != nil { // #nosec G204 -- fixed command, validated config path
			logging.Warn("could not apply storage ACL for %s on %s: %v: %s", qemu, storageDir, err, strings.TrimSpace(string(out)))
			return nil
		}
	}
	return nil
}

// qemuUser returns the account configured in qemu.conf, with the two common
// distro defaults as a fallback. An empty result means this host does not use
// a separate qemu account (or qemu.conf is unavailable).
func qemuUser() string {
	if data, err := os.ReadFile(qemuConfigPath); err == nil {
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(key) != "user" {
				continue
			}
			if value = strings.Trim(strings.TrimSpace(value), `"'`); value != "" {
				return value
			}
		}
	}
	for _, candidate := range []string{"libvirt-qemu", "qemu"} {
		if _, err := user.Lookup(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// dynamicOwnershipEnabled parses the small subset of qemu.conf that matters
// here. libvirt defaults to enabled when the key is absent or commented out.
func dynamicOwnershipEnabled(data []byte) bool {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "dynamic_ownership" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		return value != "0"
	}
	return true
}

// RestrictDisk makes a disk or seed image owner-only. The deploy user owns the
// file before libvirt starts the domain, but dynamic ownership can chown it to
// qemu before this runs; the deploy account has passwordless sudo by design,
// so fall back to sudo rather than leaving the image world-readable.
func RestrictDisk(path string) error {
	if err := os.Chmod(path, 0600); err == nil {
		return nil
	}
	// A PXE disk is created by virt-install and may already have been chowned
	// to qemu. If the inherited/default ACL left no world permission, it is
	// already safe even though the deploy user can no longer chmod it.
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0007 == 0 {
		return nil
	}
	out, err := exec.Command("sudo", "-n", "chmod", "0600", path).CombinedOutput() // #nosec G204 -- fixed command, path from validated config
	if err != nil {
		return fmt.Errorf("restrict permissions on %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
