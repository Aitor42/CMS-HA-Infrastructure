package initvms

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/utils"
)

// Options holds flags for the init-vms phase.
type Options struct {
	JumpstartOnly bool
	NodesOnly     bool
	Cleanup       bool
	Recreate      bool
}

// Phase implements the VM initialization phase (Phase 00).
//
// MIGRATION STATUS: Ansible (ansible/playbooks/provision.yml,
// generated from config.yaml by `cms-ha inventory`) is the declared
// infrastructure-as-code path that creates and declares libvirt networks,
// disks and domains. This Go/virsh creation phase is kept on purpose during
// the coexistence window so the existing deploy pipeline keeps working until
// the Ansible playbook has been validated against a real hypervisor. Once
// that is confirmed, this creation path is removed and only the thin runtime
// libvirt primitive (start/stop/shrink, fencing) remains. Do not add new
// creation logic here — put it in the Ansible roles instead.
type Phase struct {
	cfg  *config.Config
	lv   *libvirt.Client
	pool *ssh.Pool
	opts Options
}

// NewPhase creates a new InitVMs Phase.
func NewPhase(cfg *config.Config, pool *ssh.Pool, lv *libvirt.Client) phases.Phase {
	return &Phase{cfg: cfg, pool: pool, lv: lv}
}

// NewPhaseWithOpts creates a new InitVMs Phase with options.
func NewPhaseWithOpts(cfg *config.Config, pool *ssh.Pool, lv *libvirt.Client, opts Options) phases.Phase {
	return &Phase{cfg: cfg, pool: pool, lv: lv, opts: opts}
}

func (p *Phase) Name() string        { return "00-init-vms" }
func (p *Phase) Description() string { return "Initialize libvirt networks and VMs" }

// Run executes the init-vms phase.
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

	// Validate hotdesk count
	if p.cfg.Nodes.Hotdesks.Count > p.cfg.Nodes.Hotdesks.Max {
		return fmt.Errorf("hotdesk count %d exceeds max %d",
			p.cfg.Nodes.Hotdesks.Count, p.cfg.Nodes.Hotdesks.Max)
	}

	// Check required commands
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

	// Check disk space
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
type autoinstallData struct {
	Hostname        string
	PasswordHash    string
	PubKey          string
	MACInternal     string
	IPInternal      string
	GatewayInternal string
	MACMain         string
	IPMain          string
	WANIfaceConfig  string
}

// deployJumpstart provisions the Jumpstart VM via cloud-init autoinstall.
func (p *Phase) deployJumpstart(ctx context.Context) error {
	logging.Info("Deploying Jumpstart VM...")

	if states, err := p.lv.DomainStates(ctx); err != nil {
		return fmt.Errorf("list domains: %w", err)
	} else if state, exists := states[p.cfg.Nodes.Jumpstart.Name]; exists {
		if state != "running" {
			if err := p.lv.Start(ctx, p.cfg.Nodes.Jumpstart.Name); err != nil {
				return fmt.Errorf("start existing Jumpstart VM: %w", err)
			}
		}
		if err := p.pool.WaitForSSH(ctx, p.cfg.Nodes.Jumpstart.IP, 3*time.Minute); err != nil {
			return fmt.Errorf("existing Jumpstart VM did not become reachable: %w", err)
		}
		logging.Info("Jumpstart VM already exists and is reachable; leaving its disk intact (use --recreate to replace it)")
		return nil
	}

	// Ensure SSH key exists
	keyPath := p.cfg.SSH.PrivateKey
	pubKey, err := p.ensureSSHKey(keyPath)
	if err != nil {
		return fmt.Errorf("SSH key setup failed: %w", err)
	}

	// Generate a random password hash for the admin user (password login disabled).
	// A failure here must abort: falling back to a static, publicly known hash
	// would ship a guessable admin credential into the cloud-init user-data.
	passHash, err := generateRandomPasswordHash()
	if err != nil {
		return fmt.Errorf("generate admin password hash: %w", err)
	}

	// Determine WAN interface config (if libvirt default network exists)
	hasDefaultNet := p.lv.NetExists(ctx, "default")
	wanIfaceConfig := ""
	if hasDefaultNet {
		wanIfaceConfig = fmt.Sprintf("\n      net2:\n        match:\n          macaddress: \"%s\"\n        dhcp4: true\n        dhcp4-overrides:\n          route-metric: 100\n          use-dns: true", p.cfg.Nodes.Jumpstart.MACWAN)
	}

	// Build autoinstall user-data from template
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/autoinstall/user-data.tmpl")
	if err != nil {
		return fmt.Errorf("failed to read autoinstall template: %w", err)
	}

	tmpl, err := template.New("autoinstall").Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("failed to parse autoinstall template: %w", err)
	}

	data := autoinstallData{
		Hostname:        p.cfg.Nodes.Jumpstart.Name,
		PasswordHash:    passHash,
		PubKey:          pubKey,
		MACInternal:     p.cfg.Nodes.Jumpstart.MACInternal,
		IPInternal:      p.cfg.Nodes.Jumpstart.IP,
		GatewayInternal: p.cfg.Network.Internal.Gateway,
		MACMain:         p.cfg.Nodes.Jumpstart.MACMain,
		IPMain:          p.cfg.Nodes.Jumpstart.IPMain,
		WANIfaceConfig:  wanIfaceConfig,
	}

	var userDataBuf bytes.Buffer
	if err := tmpl.Execute(&userDataBuf, data); err != nil {
		return fmt.Errorf("failed to render autoinstall template: %w", err)
	}

	// Create autoinstall directory and write files
	autoinstallDir := filepath.Join(p.cfg.VM.StorageDir, "autoinstall")
	if err := os.MkdirAll(autoinstallDir, 0700); err != nil {
		return fmt.Errorf("failed to create autoinstall dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(autoinstallDir, "user-data"), userDataBuf.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to write user-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(autoinstallDir, "meta-data"), []byte(""), 0600); err != nil {
		return fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Create seed ISO
	seedISO := filepath.Join(p.cfg.VM.StorageDir, "seed.iso")
	if err := libvirt.CreateISO(ctx, seedISO, autoinstallDir, "cidata"); err != nil {
		return fmt.Errorf("failed to create seed ISO: %w", err)
	}
	_ = os.Chmod(seedISO, 0644)
	logging.Success("Cloud-init seed ISO created: %s", seedISO)

	// Find Ubuntu 24.04 ISO
	isoPath, err := findUbuntuISO(p.cfg.VM.StorageDir)
	if err != nil {
		return err
	}
	logging.Info("Using Ubuntu ISO: %s", isoPath)

	// Create main disk
	diskPath := filepath.Join(p.cfg.VM.StorageDir, "jumpstart.qcow2")
	if err := utils.EnsureDiskPathAvailable(diskPath); err != nil {
		return err
	}
	if err := libvirt.CreateDisk(ctx, diskPath, p.cfg.Nodes.Jumpstart.DiskGB, "qcow2"); err != nil {
		return fmt.Errorf("failed to create jumpstart disk: %w", err)
	}
	_ = os.Chmod(diskPath, 0666)

	// Determine networks (always internal + main, optionally WAN)
	nets := []libvirt.NetworkOpt{
		{Network: "internal", Mac: p.cfg.Nodes.Jumpstart.MACInternal},
		{Network: "main", Mac: p.cfg.Nodes.Jumpstart.MACMain},
	}
	if hasDefaultNet {
		nets = append(nets, libvirt.NetworkOpt{Network: "default", Mac: p.cfg.Nodes.Jumpstart.MACWAN})
	}

	// Install Jumpstart VM
	logging.Info("Starting Jumpstart automated installation (this may take several minutes)...")
	err = p.lv.VirtInstall(ctx, libvirt.VirtInstallOpts{
		Name:  p.cfg.Nodes.Jumpstart.Name,
		RAM:   p.cfg.Nodes.Jumpstart.RAMInstallMB,
		VCPUs: p.cfg.Nodes.Jumpstart.VCPUs,
		Disks: []libvirt.DiskOpt{
			{Path: diskPath},
			{Path: isoPath},
			{Path: seedISO},
		},
		Networks:      nets,
		Location:      isoPath + ",kernel=casper/vmlinuz,initrd=casper/initrd",
		ExtraArgs:     "autoinstall ds=nocloud console=ttyS0,115200n8",
		OSVariant:     "ubuntu24.04",
		GraphicsNone:  true,
		Wait:          -1,
		NoReboot:      true,
		NoAutoConsole: true,
	})
	if err != nil {
		return fmt.Errorf("jumpstart virt-install failed: %w", err)
	}

	// Start the VM after installation
	logging.Info("Starting Jumpstart VM...")
	if err := p.lv.Start(ctx, p.cfg.Nodes.Jumpstart.Name); err != nil {
		logging.Warn("Failed to start jumpstart (may already be running): %v", err)
	}

	logging.Info("Waiting for Jumpstart VM to boot and SSH to become ready...")
	if err := p.pool.WaitForSSH(ctx, p.cfg.Nodes.Jumpstart.IP, 3*time.Minute); err != nil {
		logging.Warn("Jumpstart VM started, but SSH is not yet ready: %v", err)
	} else {
		logging.Success("Jumpstart VM is reachable via SSH at %s", p.cfg.Nodes.Jumpstart.IP)
	}

	logging.Success("Jumpstart VM deployed and started")
	return nil
}

// deployClientNodes provisions all client nodes via PXE boot.
func (p *Phase) deployClientNodes(ctx context.Context) error {
	logging.Info("Deploying client nodes...")

	// The jumpstart is installed from its ISO rather than over PXE, so it is
	// the one entry of the topology this phase must leave alone.
	var clientNames []string
	for _, node := range p.cfg.AllNodes() {
		if node.Name == "" || node.Name == p.cfg.Nodes.Jumpstart.Name {
			continue
		}
		if err := utils.CreateClientVM(ctx, p.cfg, p.lv, node.Name); err != nil {
			return fmt.Errorf("failed to create VM %s: %w", node.Name, err)
		}
		clientNames = append(clientNames, node.Name)
	}

	// virt-install 4.1 boots a freshly created guest from the network first,
	// even when --boot asks for the disk, so the post-install reboot would
	// re-enter PXE and reinstall the node. Normalise the order once every
	// client domain is defined; the change applies on their next boot, which
	// is the reboot the installer performs minutes later.
	if err := utils.FixBootOrder(ctx, p.lv, clientNames); err != nil {
		return fmt.Errorf("fix client node boot order: %w", err)
	}

	logging.Success("All client nodes deployed")
	return nil
}

// CleanupOptions specifies options for the cleanup operation.
type CleanupOptions struct {
	VMsOnly      bool
	NetworksOnly bool
}

// Cleanup destroys all VMs and/or virtual networks managed by the infrastructure.
func Cleanup(ctx context.Context, cfg *config.Config, lv *libvirt.Client, opts CleanupOptions) error {
	p := &Phase{cfg: cfg, lv: lv}
	return p.cleanupWithOptions(ctx, opts)
}

// cleanup destroys all VMs and networks.
func (p *Phase) cleanup(ctx context.Context) error {
	return p.cleanupWithOptions(ctx, CleanupOptions{})
}

func (p *Phase) cleanupWithOptions(ctx context.Context, opts CleanupOptions) error {
	if opts.VMsOnly && opts.NetworksOnly {
		return fmt.Errorf("VMsOnly and NetworksOnly are mutually exclusive")
	}

	timer := logging.PhaseStart("Cleanup VMs and Networks")
	defer timer.End()

	var errs []error

	if !opts.NetworksOnly {
		logging.Info("Destroying all VMs and disk images...")
		allNodes := p.cfg.AllNodes()
		// One `virsh list --all` snapshot: a libvirt query failure is not
		// evidence the domains are absent, so abort VM destruction rather than
		// risk deleting the disks of live VMs.
		states, err := p.lv.DomainStates(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list domains: %w", err))
		} else {
			for _, node := range allNodes {
				if node.Name == "" {
					continue
				}
				state, exists := states[node.Name]
				if err := p.destroyVM(ctx, node.Name, state, exists); err != nil {
					errs = append(errs, fmt.Errorf("remove VM %s: %w", node.Name, err))
				}
			}
		}
	}

	if !opts.VMsOnly {
		logging.Info("Destroying virtual networks...")
		for _, net := range []string{"internal", "main"} {
			exists, err := p.lv.NetExistsChecked(ctx, net)
			if err != nil {
				errs = append(errs, fmt.Errorf("check network %s: %w", net, err))
				continue
			}
			if !exists {
				continue
			}
			_ = p.lv.NetDestroy(ctx, net) // An inactive network may already be stopped.
			if err := p.lv.NetUndefine(ctx, net); err != nil {
				errs = append(errs, fmt.Errorf("undefine network %s: %w", net, err))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}
	logging.Success("Cleanup completed")
	return nil
}

// destroyVM safely destroys and undefines a VM if it exists, and cleans up its
// disks. exists/state come from a single caller-side domain snapshot.
func (p *Phase) destroyVM(ctx context.Context, name, state string, exists bool) error {
	if exists {
		logging.Info("Removing existing VM: %s", name)
		if state != "shut off" && state != "crashed" {
			if err := p.lv.Destroy(ctx, name); err != nil {
				return fmt.Errorf("stop domain: %w", err)
			}
		}
		if err := p.lv.Undefine(ctx, name); err != nil {
			return fmt.Errorf("undefine domain: %w", err)
		}
	}
	for _, disk := range []string{
		filepath.Join(p.cfg.VM.StorageDir, name+".qcow2"),
		filepath.Join(p.cfg.VM.StorageDir, name+"-drbd.qcow2"),
	} {
		if err := os.Remove(disk); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove disk %s: %w", disk, err)
		}
	}
	if name == p.cfg.Nodes.Jumpstart.Name {
		seed := filepath.Join(p.cfg.VM.StorageDir, "seed.iso")
		if err := os.Remove(seed); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove seed ISO: %w", err)
		}
	}
	return nil
}

// ensureSSHKey returns the public key for the given private key path, generating it if needed.
func (p *Phase) ensureSSHKey(keyPath string) (string, error) {
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		logging.Info("Generating SSH key at %s...", keyPath)
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return "", fmt.Errorf("failed to create SSH dir: %w", err)
		}
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", filepath.Clean(keyPath)) // #nosec G204 -- keyPath is within configured safe storage directory
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen failed: %v: %s", err, out)
		}
	}

	cleanedKeyPath := filepath.Clean(keyPath + ".pub")
	pubKeyBytes, err := os.ReadFile(cleanedKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}
	return strings.TrimSpace(string(pubKeyBytes)), nil
}

// findUbuntuISO searches for an Ubuntu 24.04 ISO in the VM storage directory.
func findUbuntuISO(dir string) (string, error) {
	candidates := []string{
		filepath.Join(dir, "ubuntu-24.04.4-live-server-amd64.iso"),
		filepath.Join(dir, "ubuntu-24.04.2-live-server-amd64.iso"),
		filepath.Join(dir, "ubuntu-24.04-live-server-amd64.iso"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Size() > 0 {
			return c, nil
		}
	}

	// Try glob
	matches, _ := filepath.Glob(filepath.Join(dir, "*ubuntu-24.04*.iso"))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Size() > 1<<30 { // >1GB
			return m, nil
		}
	}

	return "", fmt.Errorf("no Ubuntu 24.04 ISO found in %s; download it first", dir)
}

// generateRandomPasswordHash generates a random SHA-512 password hash using crypto/rand.
// The actual password is random and discarded; SSH key auth is enforced and late-commands locks the accounts.
func generateRandomPasswordHash() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", fmt.Errorf("generate random password: %w", err)
	}
	randSecret := hex.EncodeToString(b)
	out, err := exec.Command("openssl", "passwd", "-6", randSecret).Output() // #nosec G204 -- openssl with generated random string
	if err != nil {
		return "", fmt.Errorf("openssl passwd -6 failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("openssl passwd -6 returned an empty hash")
	}
	return hash, nil
}
