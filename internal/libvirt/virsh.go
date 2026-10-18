package libvirt

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client is a wrapper around virsh and virt-install CLI commands.
type Client struct {
	URI     string
	UseSudo bool
}

// DomainInfo holds information about a libvirt domain.
type DomainInfo struct {
	Name  string
	State string
}

// DiskOpt represents a disk option for virt-install.
type DiskOpt struct {
	Path   string
	SizeGB int
	Format string
	Bus    string
	Device string
}

// NetworkOpt represents a network option for virt-install.
type NetworkOpt struct {
	Network string
	Mac     string
}

// VirtInstallOpts holds all parameters for virt-install.
type VirtInstallOpts struct {
	Name          string
	RAM           int // in MB
	VCPUs         int
	Disks         []DiskOpt
	Networks      []NetworkOpt
	PXE           bool
	BootOrder     []string
	OSVariant     string
	ExtraArgs     string
	Location      string
	Wait          int
	NoAutoConsole bool
	NoReboot      bool
	GraphicsNone  bool
}

// probeTimeout bounds the client's sudo/URI probe so an unreachable
// qemu+ssh:// host does not hang CLI startup for minutes.
const probeTimeout = 5 * time.Second

// NewClient creates a new Client and auto-detects if sudo is needed.
func NewClient(uri string) *Client {
	useSudo := false
	if _, err := exec.LookPath("virsh"); err == nil {
		if uri == "qemu:///system" || strings.HasPrefix(uri, "qemu+ssh://root@") {
			// Check if virsh works directly
			probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			if err := exec.CommandContext(probeCtx, "virsh", "-c", uri, "list").Run(); err != nil { // #nosec G204
				// Only use sudo if sudo -n works non-interactively
				if errSudo := exec.CommandContext(probeCtx, "sudo", "-n", "virsh", "-c", uri, "list").Run(); errSudo == nil { // #nosec G204
					useSudo = true
				}
			}
			cancel()
		}
	}

	return &Client{
		URI:     uri,
		UseSudo: useSudo,
	}
}

func (c *Client) virsh(ctx context.Context, args ...string) ([]byte, error) {
	cmdArgs := []string{"-c", c.URI}
	cmdArgs = append(cmdArgs, args...)

	var cmd *exec.Cmd
	if c.UseSudo {
		sudoArgs := append([]string{"-n", "virsh"}, cmdArgs...)
		cmd = exec.CommandContext(ctx, "sudo", sudoArgs...) // #nosec G204 -- virsh command with validated arguments
	} else {
		cmd = exec.CommandContext(ctx, "virsh", cmdArgs...) // #nosec G204 -- virsh command with validated arguments
	}

	cmd.Env = append(cmd.Environ(), "LIBVIRT_DEFAULT_URI="+c.URI)

	slog.Debug("executing virsh command", "args", cmdArgs)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("virsh command failed: %v, output: %s", err, string(out))
	}
	return out, nil
}

// DomainState returns the state of the domain (e.g., "running", "shut off").
func (c *Client) DomainState(ctx context.Context, name string) (string, error) {
	out, err := c.virsh(ctx, "domstate", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// DomainExists checks if a domain exists.
func (c *Client) DomainExists(ctx context.Context, name string) bool {
	_, err := c.DomainState(ctx, name)
	return err == nil
}

// DomainExistsChecked distinguishes an absent domain from a failed virsh query.
func (c *Client) DomainExistsChecked(ctx context.Context, name string) (bool, error) {
	out, err := c.virsh(ctx, "list", "--all", "--name")
	if err != nil {
		return false, err
	}
	for _, domain := range strings.Split(string(out), "\n") {
		domain = strings.TrimSpace(domain)
		// Empty output lines (notably the trailing newline) must not match an
		// empty or placeholder node name and report a domain that isn't there.
		if domain == "" {
			continue
		}
		if domain == name {
			return true, nil
		}
	}
	return false, nil
}

// DomainStates returns a snapshot of every defined domain mapped to its state
// with a single `virsh list --all` invocation, avoiding one `virsh domstate`
// subprocess per VM.
func (c *Client) DomainStates(ctx context.Context) (map[string]string, error) {
	domains, err := c.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	states := make(map[string]string, len(domains))
	for _, d := range domains {
		states[strings.TrimSpace(d.Name)] = strings.TrimSpace(d.State)
	}
	return states, nil
}

// Start starts a domain.
func (c *Client) Start(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "start", name)
	return err
}

// Shutdown performs an ACPI shutdown of a domain.
func (c *Client) Shutdown(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "shutdown", name)
	return err
}

// Resume resumes a paused domain so it can process an ACPI shutdown request.
func (c *Client) Resume(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "resume", name)
	return err
}

// Destroy forces power off of a domain.
func (c *Client) Destroy(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "destroy", name)
	return err
}

// Undefine undefines a domain.
func (c *Client) Undefine(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "undefine", name)
	return err
}

// SetMaxMemory sets the maximum memory for a domain.
func (c *Client) SetMaxMemory(ctx context.Context, name string, kib int64) error {
	_, err := c.virsh(ctx, "setmaxmem", name, fmt.Sprintf("%d", kib), "--config")
	return err
}

// SetMemory sets the memory for a domain.
func (c *Client) SetMemory(ctx context.Context, name string, kib int64) error {
	_, err := c.virsh(ctx, "setmem", name, fmt.Sprintf("%d", kib), "--config")
	return err
}

// DumpXML dumps the XML configuration of a domain.
func (c *Client) DumpXML(ctx context.Context, name string) (string, error) {
	out, err := c.virsh(ctx, "dumpxml", name)
	return string(out), err
}

// Define defines a domain from an XML configuration.
func (c *Client) Define(ctx context.Context, xmlPath string) error {
	_, err := c.virsh(ctx, "define", xmlPath)
	return err
}

// ListAll returns all domains.
func (c *Client) ListAll(ctx context.Context) ([]DomainInfo, error) {
	out, err := c.virsh(ctx, "list", "--all")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(out), "\n")
	var domains []DomainInfo
	for i, line := range lines {
		if i < 2 || strings.TrimSpace(line) == "" {
			continue // skip header
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			domains = append(domains, DomainInfo{
				Name:  fields[1],
				State: strings.Join(fields[2:], " "),
			})
		}
	}
	return domains, nil
}

// ListRunning returns running domains.
func (c *Client) ListRunning(ctx context.Context) ([]string, error) {
	domains, err := c.ListAll(ctx)
	if err != nil {
		return nil, err
	}

	var running []string
	for _, d := range domains {
		if strings.EqualFold(strings.TrimSpace(d.State), "running") {
			running = append(running, d.Name)
		}
	}
	return running, nil
}

// NetDefine defines a network from an XML file.
func (c *Client) NetDefine(ctx context.Context, xmlPath string) error {
	_, err := c.virsh(ctx, "net-define", xmlPath)
	return err
}

// NetStart starts a network.
func (c *Client) NetStart(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "net-start", name)
	return err
}

// NetAutostart sets autostart for a network.
func (c *Client) NetAutostart(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "net-autostart", name)
	return err
}

// NetDestroy stops a network.
func (c *Client) NetDestroy(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "net-destroy", name)
	return err
}

// NetUndefine undefines a network.
func (c *Client) NetUndefine(ctx context.Context, name string) error {
	_, err := c.virsh(ctx, "net-undefine", name)
	return err
}

// NetExists checks if a network exists.
func (c *Client) NetExists(ctx context.Context, name string) bool {
	_, err := c.virsh(ctx, "net-info", name)
	return err == nil
}

// NetExistsChecked distinguishes an absent network from a failed virsh query.
func (c *Client) NetExistsChecked(ctx context.Context, name string) (bool, error) {
	out, err := c.virsh(ctx, "net-list", "--all", "--name")
	if err != nil {
		return false, err
	}
	for _, network := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(network) == name {
			return true, nil
		}
	}
	return false, nil
}

// NetIsActive checks if a libvirt network is active.
func (c *Client) NetIsActive(ctx context.Context, name string) bool {
	out, err := c.virsh(ctx, "net-info", name)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Active:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.EqualFold(fields[1], "yes") {
				return true
			}
		}
	}
	return false
}

// VirtInstall builds and executes a virt-install command.
func (c *Client) VirtInstall(ctx context.Context, opts VirtInstallOpts) error {
	// virt-install joins --disk/--network specs with commas; values containing
	// one would silently corrupt the parsed options.
	for i, d := range opts.Disks {
		for field, v := range map[string]string{"path": d.Path, "format": d.Format, "bus": d.Bus, "device": d.Device} {
			if strings.Contains(v, ",") {
				return fmt.Errorf("disk %d: %s must not contain commas: %q", i+1, field, v)
			}
		}
	}
	for i, n := range opts.Networks {
		for field, v := range map[string]string{"network": n.Network, "mac": n.Mac} {
			if strings.Contains(v, ",") {
				return fmt.Errorf("network %d: %s must not contain commas: %q", i+1, field, v)
			}
		}
	}

	args := []string{"--connect", c.URI, "--name", opts.Name, "--memory", fmt.Sprintf("%d", opts.RAM), "--vcpus", fmt.Sprintf("%d", opts.VCPUs)}

	if opts.OSVariant != "" {
		args = append(args, "--os-variant", opts.OSVariant)
	}

	for _, d := range opts.Disks {
		var parts []string
		parts = append(parts, "path="+d.Path)
		if d.SizeGB > 0 {
			parts = append(parts, fmt.Sprintf("size=%d", d.SizeGB))
		}
		if d.Format != "" {
			parts = append(parts, "format="+d.Format)
		} else if d.SizeGB > 0 || strings.HasSuffix(d.Path, ".qcow2") {
			parts = append(parts, "format=qcow2")
		}
		if d.Bus != "" {
			parts = append(parts, "bus="+d.Bus)
		} else if d.Device == "cdrom" || strings.HasSuffix(d.Path, ".iso") {
			parts = append(parts, "bus=sata")
		} else {
			parts = append(parts, "bus=virtio")
		}
		if d.Device != "" {
			parts = append(parts, "device="+d.Device)
		} else if strings.HasSuffix(d.Path, ".iso") {
			parts = append(parts, "device=cdrom")
		}
		args = append(args, "--disk", strings.Join(parts, ","))
	}

	for _, n := range opts.Networks {
		netArg := fmt.Sprintf("network=%s,model=virtio", n.Network)
		if n.Mac != "" {
			netArg += fmt.Sprintf(",mac=%s", n.Mac)
		}
		args = append(args, "--network", netArg)
	}

	if opts.PXE && len(opts.BootOrder) == 0 {
		args = append(args, "--pxe")
	}

	if len(opts.BootOrder) > 0 {
		args = append(args, "--boot", strings.Join(opts.BootOrder, ","))
	}

	if opts.Location != "" {
		args = append(args, "--location", opts.Location)
	}

	if opts.ExtraArgs != "" {
		args = append(args, "--extra-args", opts.ExtraArgs)
	}

	args = append(args, "--wait", strconv.Itoa(opts.Wait))

	if opts.NoAutoConsole {
		args = append(args, "--noautoconsole")
	}

	if opts.NoReboot {
		args = append(args, "--noreboot")
	}

	if opts.GraphicsNone {
		args = append(args, "--graphics", "none")
	}

	var cmd *exec.Cmd
	if c.UseSudo {
		sudoArgs := append([]string{"-n", "virt-install"}, args...)
		cmd = exec.CommandContext(ctx, "sudo", sudoArgs...) // #nosec G204 -- virt-install command with structured arguments
	} else {
		cmd = exec.CommandContext(ctx, "virt-install", args...) // #nosec G204 -- virt-install command with structured arguments
	}

	slog.Debug("executing virt-install", "args", args)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virt-install failed: %v, output: %s", err, string(out))
	}

	return nil
}

// CreateDisk runs qemu-img create.
func CreateDisk(ctx context.Context, path string, sizeGB int, format string) error {
	if path == "" {
		return fmt.Errorf("disk path is required")
	}
	if format == "" {
		return fmt.Errorf("disk format is required (use qcow2)")
	}
	args := []string{"create", "-f", format, path, fmt.Sprintf("%dG", sizeGB)}
	cmd := exec.CommandContext(ctx, "qemu-img", args...) // #nosec G204 -- qemu-img command with validated options
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create failed: %v, output: %s", err, string(out))
	}
	return nil
}

// CreateISO runs xorrisofs to create an ISO.
func CreateISO(ctx context.Context, outputPath, sourceDir, volID string) error {
	if outputPath == "" || sourceDir == "" {
		return fmt.Errorf("ISO output path and source directory are required")
	}
	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		// Failing to clear the old output would make xorrisofs fail with a
		// cryptic error; surface the real cause instead.
		return fmt.Errorf("failed to remove previous ISO %s: %w", outputPath, err)
	}
	args := []string{"-o", outputPath, "-V", volID, "-R", "-J", sourceDir}
	cmd := exec.CommandContext(ctx, "xorrisofs", args...) // #nosec G204 -- xorrisofs command with validated options
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xorrisofs failed: %v, output: %s", err, string(out))
	}
	return nil
}
