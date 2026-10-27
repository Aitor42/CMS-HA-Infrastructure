package initvms

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/hypervisor"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// Options holds flags for the init-vms phase.
type Options struct {
	JumpstartOnly bool
	NodesOnly     bool
	Cleanup       bool
	Recreate      bool
	// JumpstartImage overrides vm.jumpstart_image for one run: "iso" or
	// "cloud". Empty keeps the configured value (ISO by default).
	JumpstartImage string
	// FastProvisioning installs client nodes from the cloud image instead of
	// PXE. It is the opt-in fast path; the PXE/Cobbler gate stays the default.
	FastProvisioning bool
}

// Phase implements the VM initialization phase (Phase 00).
//
// This Go/libvirt phase is the supported default and the path exercised by the
// PXE/Cobbler gate. Ansible (ansible/playbooks/provision.yml, generated from
// config.yaml by `cms-ha inventory`) is an optional declarative alternative;
// it is linted and parity-tested, but not the gate-validated path. Keep the two
// paths driven by the same config.yaml rather than inventing independent
// addresses or sizes.
type Phase struct {
	cfg  *config.Config
	lv   *libvirt.Client
	pool ssh.Executor
	opts Options
}

// NewPhase creates a new InitVMs Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor, lv *libvirt.Client) phases.Phase {
	return &Phase{cfg: cfg, pool: pool, lv: lv}
}

// NewPhaseWithOpts creates a new InitVMs Phase with options.
func NewPhaseWithOpts(cfg *config.Config, pool ssh.Executor, lv *libvirt.Client, opts Options) phases.Phase {
	return &Phase{cfg: cfg, pool: pool, lv: lv, opts: opts}
}

func (p *Phase) Name() string        { return catalog.InitVMs.Name }
func (p *Phase) Description() string { return catalog.InitVMs.Description }

// Run provisions the libvirt networks and VMs, or removes them when cleanup is
// requested.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	// Cleanup only talks to libvirt, so it must not require the VM-creation
	// tooling (virt-install, xorrisofs, qemu-img) nor a valid hotdesk count:
	// tearing down has to work even on a host that can no longer deploy.
	if !p.opts.Cleanup {
		if err := p.preflightChecks(ctx); err != nil {
			return fmt.Errorf("preflight checks failed: %w", err)
		}
	}

	if p.opts.Cleanup {
		return p.cleanup(ctx)
	}

	if p.opts.Recreate {
		logging.Info("Recreate mode: cleaning existing resources first...")
		if err := p.cleanup(ctx); err != nil {
			return fmt.Errorf("cleanup before recreate: %w", err)
		}
	}

	// Default: deploy both jumpstart and nodes unless filtered
	deployJumpstart := p.opts.JumpstartOnly || (!p.opts.JumpstartOnly && !p.opts.NodesOnly)
	deployNodes := p.opts.NodesOnly || (!p.opts.JumpstartOnly && !p.opts.NodesOnly)

	if deployJumpstart || deployNodes {
		if err := p.setupNetworks(ctx); err != nil {
			return fmt.Errorf("network setup failed: %w", err)
		}
	}

	if deployJumpstart {
		if err := p.deployJumpstart(ctx); err != nil {
			return fmt.Errorf("jumpstart deployment failed: %w", err)
		}
	}

	if deployNodes {
		if err := p.deployClientNodes(ctx); err != nil {
			return fmt.Errorf("client node deployment failed: %w", err)
		}
	}

	return nil
}

// preflightChecks validates prerequisites before deployment.
func (p *Phase) preflightChecks(ctx context.Context) error {
	logging.Info("Running preflight checks...")

	if p.cfg.Nodes.Hotdesks.Count > p.cfg.Nodes.Hotdesks.Max {
		return fmt.Errorf("hotdesk count %d exceeds max %d",
			p.cfg.Nodes.Hotdesks.Count, p.cfg.Nodes.Hotdesks.Max)
	}

	required := map[string]string{
		"virsh":        "libvirt-daemon-system",
		"virt-install": "virtinst",
		"xorrisofs":    "xorriso",
		"qemu-img":     "qemu-utils",
	}
	for cmd, pkg := range required {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("%q not found in PATH; install: apt install %s", cmd, pkg)
		}
	}
	if err := hypervisor.CheckDiskAccess(p.lv); err != nil {
		return err
	}

	dir := filepath.Clean(p.cfg.VM.StorageDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create storage dir %s: %w", dir, err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err == nil && stat.Bsize > 0 {
		freeGB := stat.Bavail * uint64(stat.Bsize) / (1 << 30)
		logging.Info("Storage: %s (%d GB free)", dir, freeGB)
		if freeGB < 30 {
			logging.Warn("Low disk space: %d GB free (>= 30 GB recommended)", freeGB)
		}
	}

	logging.Success("Preflight checks passed")
	return nil
}

// setupNetworks creates and activates the internal and main virtual networks,
// and configures host bridge IPs for local cluster routing.
func (p *Phase) setupNetworks(ctx context.Context) error {
	logging.Info("Setting up virtual networks...")

	type netDef struct {
		name       string
		tmplPath   string
		bridge     string
		hostCIDR   string
		routerCIDR string
	}
	// Derive the bridge addresses from the configured CIDRs instead of
	// hardcoding 192.168.10/20.0/24: the host takes .254 and the router uses the
	// configured gateway (falling back to .1).
	linkCIDR := func(gateway, prefix string) string {
		if gateway != "" {
			return gateway + "/24"
		}
		return prefix + "1/24"
	}
	internalPrefix := config.PrefixFromCIDR(p.cfg.Network.Internal.CIDR, "192.168.10.")
	mainPrefix := config.PrefixFromCIDR(p.cfg.Network.Main.CIDR, "192.168.20.")
	nets := []netDef{
		{
			name:       "internal",
			tmplPath:   "templates/libvirt/internal-net.xml",
			bridge:     p.cfg.Network.Internal.Bridge,
			hostCIDR:   internalPrefix + "254/24",
			routerCIDR: linkCIDR(p.cfg.Network.Internal.Gateway, internalPrefix),
		},
		{
			name:       "main",
			tmplPath:   "templates/libvirt/main-net.xml",
			bridge:     p.cfg.Network.Main.Bridge,
			hostCIDR:   mainPrefix + "254/24",
			routerCIDR: linkCIDR(p.cfg.Network.Main.Gateway, mainPrefix),
		},
	}

	for _, nd := range nets {
		if !p.lv.NetExists(ctx, nd.name) {
			content, err := cms.TemplatesFS.ReadFile(nd.tmplPath)
			if err != nil {
				return fmt.Errorf("failed to read network template %s: %w", nd.tmplPath, err)
			}

			tmpFile, err := os.CreateTemp("", nd.name+"-net-*.xml")
			if err != nil {
				return fmt.Errorf("failed to create temp file for network XML: %w", err)
			}
			tmpPath := tmpFile.Name()
			if _, err := tmpFile.Write(content); err != nil {
				tmpFile.Close()
				os.Remove(tmpPath)
				return fmt.Errorf("failed to write network XML: %w", err)
			}
			tmpFile.Close()
			err = p.lv.NetDefine(ctx, tmpPath)
			os.Remove(tmpPath)
			if err != nil {
				return fmt.Errorf("failed to define network %s: %w", nd.name, err)
			}
			if err := p.lv.NetStart(ctx, nd.name); err != nil {
				return fmt.Errorf("failed to start network %s: %w", nd.name, err)
			}
			if err := p.lv.NetAutostart(ctx, nd.name); err != nil {
				logging.Warn("Failed to set autostart for network %s: %v", nd.name, err)
			}
			logging.Success("Network %q created and started", nd.name)
		} else {
			if !p.lv.NetIsActive(ctx, nd.name) {
				if err := p.lv.NetStart(ctx, nd.name); err != nil {
					return fmt.Errorf("failed to start network %s: %w", nd.name, err)
				}
				logging.Success("Network %q started", nd.name)
			} else {
				logging.Info("Network %q is active", nd.name)
			}
		}

		bridge := nd.bridge
		if bridge == "" {
			if nd.name == "internal" {
				bridge = "virbr-int"
			} else {
				bridge = "virbr-main"
			}
		}
		if err := configureHostBridge(ctx, bridge, nd.hostCIDR, nd.routerCIDR); err != nil {
			logging.Warn("Could not configure host IP %s on bridge %s: %v", nd.hostCIDR, bridge, err)
		}
	}

	return nil
}

// configureHostBridge assigns a .254 IP address to a host bridge interface to allow host routing to VMs.
func configureHostBridge(ctx context.Context, bridge, targetCIDR, routerCIDR string) error {
	cmd := exec.CommandContext(ctx, "ip", "-o", "-4", "addr", "show", "dev", bridge)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to check bridge %s: %w", bridge, err)
	}

	output := string(out)
	if strings.Contains(output, targetCIDR) {
		logging.Info("Bridge %s already has IP %s", bridge, targetCIDR)
		return nil
	}

	logging.Info("Assigning IP %s to host bridge %s...", targetCIDR, bridge)

	runNetCmd := func(args ...string) error {
		var c *exec.Cmd
		if os.Geteuid() == 0 {
			c = exec.CommandContext(ctx, "ip", args...)
		} else {
			sudoArgs := append([]string{"-n", "ip"}, args...)
			c = exec.CommandContext(ctx, "sudo", sudoArgs...)
		}
		if cOut, cErr := c.CombinedOutput(); cErr != nil {
			return fmt.Errorf("ip %s failed: %w: %s", strings.Join(args, " "), cErr, string(cOut))
		}
		return nil
	}

	// Remove conflicting router IP (.1) if accidentally bound to host bridge
	if routerCIDR != "" && strings.Contains(output, routerCIDR) {
		_ = runNetCmd("addr", "del", routerCIDR, "dev", bridge)
	}

	if err := runNetCmd("addr", "add", targetCIDR, "dev", bridge); err != nil {
		return err
	}
	if err := runNetCmd("link", "set", "dev", bridge, "up"); err != nil {
		return err
	}

	logging.Success("Bridge %s configured with IP %s", bridge, targetCIDR)
	return nil
}

// autoinstallData holds data for the cloud-init user-data template.
