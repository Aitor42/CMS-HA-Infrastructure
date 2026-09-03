package libvirt

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// Client is a wrapper around virsh and virt-install CLI commands.
type Client struct {
	URI     string
	UseSudo bool
}

// DomainInfo holds information about a libvirt domain.
type DomainInfo struct {
	ID    string
	Name  string
	State string
}

// DiskOpt represents a disk option for virt-install.
type DiskOpt struct {
	Path   string
	SizeGB int
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

// NewClient creates a new Client and auto-detects if sudo is needed.
func NewClient(uri string) *Client {
	useSudo := false
	if uri == "qemu:///system" || strings.HasPrefix(uri, "qemu+ssh://root@") {
		// Just a heuristic. In a real system, you could check if current user is in libvirt group.
		err := exec.Command("virsh", "-c", uri, "list").Run()
		if err != nil {
			useSudo = true
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
		sudoArgs := append([]string{"virsh"}, cmdArgs...)
		cmd = exec.CommandContext(ctx, "sudo", sudoArgs...)
	} else {
		cmd = exec.CommandContext(ctx, "virsh", cmdArgs...)
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

// SetVCPUs sets the vCPU count for a domain.
func (c *Client) SetVCPUs(ctx context.Context, name string, count int) error {
	_, err := c.virsh(ctx, "setvcpus", name, fmt.Sprintf("%d", count), "--config", "--maximum")
	if err != nil {
		return err
	}
	_, err = c.virsh(ctx, "setvcpus", name, fmt.Sprintf("%d", count), "--config")
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
				ID:    fields[0],
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
		if d.State == "running" {
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

// VirtInstall builds and executes a virt-install command.
func (c *Client) VirtInstall(ctx context.Context, opts VirtInstallOpts) error {
	args := []string{"--connect", c.URI, "--name", opts.Name, "--memory", fmt.Sprintf("%d", opts.RAM), "--vcpus", fmt.Sprintf("%d", opts.VCPUs)}
	
	if opts.OSVariant != "" {
		args = append(args, "--os-variant", opts.OSVariant)
	}

	for _, d := range opts.Disks {
		if d.SizeGB > 0 {
			args = append(args, "--disk", fmt.Sprintf("path=%s,size=%d,format=qcow2,bus=virtio", d.Path, d.SizeGB))
		} else {
			args = append(args, "--disk", fmt.Sprintf("path=%s", d.Path))
		}
	}
	
	for _, n := range opts.Networks {
		netArg := fmt.Sprintf("network=%s,model=virtio", n.Network)
		if n.Mac != "" {
			netArg += fmt.Sprintf(",mac=%s", n.Mac)
		}
		args = append(args, "--network", netArg)
	}

	if opts.PXE {
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

	if opts.Wait != 0 {
		args = append(args, "--wait", strconv.Itoa(opts.Wait))
	}

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
		sudoArgs := append([]string{"virt-install"}, args...)
		cmd = exec.CommandContext(ctx, "sudo", sudoArgs...)
	} else {
		cmd = exec.CommandContext(ctx, "virt-install", args...)
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
	args := []string{"create", "-f", format, path, fmt.Sprintf("%dG", sizeGB)}
	cmd := exec.CommandContext(ctx, "qemu-img", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create failed: %v, output: %s", err, string(out))
	}
	return nil
}

// CreateISO runs xorrisofs to create an ISO.
func CreateISO(ctx context.Context, outputPath, sourceDir, volID string) error {
	args := []string{"-o", outputPath, "-V", volID, "-R", "-J", sourceDir}
	cmd := exec.CommandContext(ctx, "xorrisofs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xorrisofs failed: %v, output: %s", err, string(out))
	}
	return nil
}
