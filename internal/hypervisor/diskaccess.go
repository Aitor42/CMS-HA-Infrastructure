package hypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
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
	out, err := exec.Command("sudo", "-n", "chmod", "0600", path).CombinedOutput() // #nosec G204 -- fixed command, path from validated config
	if err != nil {
		return fmt.Errorf("restrict permissions on %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
