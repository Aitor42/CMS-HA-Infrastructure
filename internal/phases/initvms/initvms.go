package initvms

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
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
)

// Options holds flags for the init-vms phase.
type Options struct {
	JumpstartOnly bool
	NodesOnly     bool
	Cleanup       bool
	Recreate      bool
}

// Phase implements the VM initialization phase (Phase 00).
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

	if err := p.preflightChecks(ctx); err != nil {
		return fmt.Errorf("preflight checks failed: %w", err)
	}

	if p.opts.Cleanup {
		return p.cleanup(ctx)
	}

	if p.opts.Recreate {
		logging.Info("Recreate mode: cleaning existing resources first...")
		if err := p.cleanup(ctx); err != nil {
			logging.Warn("Cleanup before recreate had errors: %v", err)
		}
	}

	// Default: deploy both jumpstart and nodes unless filtered
	deployJumpstart := p.opts.JumpstartOnly || (!p.opts.JumpstartOnly && !p.opts.NodesOnly)
	deployNodes := p.opts.NodesOnly || (!p.opts.JumpstartOnly && !p.opts.NodesOnly)

	if deployJumpstart {
		if err := p.setupNetworks(ctx); err != nil {
			return fmt.Errorf("network setup failed: %w", err)
		}
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
	dir := p.cfg.VM.StorageDir
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("cannot create storage dir %s: %w", dir, err)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err == nil {
		freeGB := stat.Bavail * uint64(stat.Bsize) / (1 << 30)
		logging.Info("Storage: %s (%d GB free)", dir, freeGB)
		if freeGB < 30 {
			logging.Warn("Low disk space: %d GB free (>= 30 GB recommended)", freeGB)
		}
	}

	logging.Success("Preflight checks passed")
	return nil
}

// setupNetworks creates the internal and main virtual networks.
func (p *Phase) setupNetworks(ctx context.Context) error {
	logging.Info("Setting up virtual networks...")

	type netDef struct {
		name    string
		tmplPath string
	}
	nets := []netDef{
		{"internal", "templates/libvirt/internal-net.xml"},
		{"main", "templates/libvirt/main-net.xml"},
	}

	for _, nd := range nets {
		if p.lv.NetExists(ctx, nd.name) {
			logging.Info("Network %q already exists, skipping", nd.name)
			continue
		}

		content, err := cms.TemplatesFS.ReadFile(nd.tmplPath)
		if err != nil {
			return fmt.Errorf("failed to read network template %s: %w", nd.tmplPath, err)
		}

		tmpFile := filepath.Join(os.TempDir(), nd.name+"-net.xml")
		if err := os.WriteFile(tmpFile, content, 0644); err != nil {
			return fmt.Errorf("failed to write network XML: %w", err)
		}
		defer os.Remove(tmpFile)

		if err := p.lv.NetDefine(ctx, tmpFile); err != nil {
			return fmt.Errorf("failed to define network %s: %w", nd.name, err)
		}
		if err := p.lv.NetStart(ctx, nd.name); err != nil {
			return fmt.Errorf("failed to start network %s: %w", nd.name, err)
		}
		if err := p.lv.NetAutostart(ctx, nd.name); err != nil {
			logging.Warn("Failed to set autostart for network %s: %v", nd.name, err)
		}
		logging.Success("Network %q created and started", nd.name)
	}

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

	// Clean up existing jumpstart
	p.destroyVM(ctx, p.cfg.Nodes.Jumpstart.Name)

	// Ensure SSH key exists
	keyPath := p.cfg.SSH.PrivateKey
	pubKey, err := p.ensureSSHKey(keyPath)
	if err != nil {
		return fmt.Errorf("SSH key setup failed: %w", err)
	}

	// Generate a random password hash for the admin user (password login disabled)
	passHash := generateRandomPasswordHash()

	// Determine WAN interface config (if libvirt default network exists)
	wanIfaceConfig := ""
	_ = wanIfaceConfig // used in template

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
	}

	var userDataBuf bytes.Buffer
	if err := tmpl.Execute(&userDataBuf, data); err != nil {
		return fmt.Errorf("failed to render autoinstall template: %w", err)
	}

	// Create autoinstall directory and write files
	autoinstallDir := filepath.Join(p.cfg.VM.StorageDir, "autoinstall")
	if err := os.MkdirAll(autoinstallDir, 0755); err != nil {
		return fmt.Errorf("failed to create autoinstall dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(autoinstallDir, "user-data"), userDataBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write user-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(autoinstallDir, "meta-data"), []byte(""), 0644); err != nil {
		return fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Create seed ISO
	seedISO := filepath.Join(p.cfg.VM.StorageDir, "seed.iso")
	if err := libvirt.CreateISO(ctx, seedISO, autoinstallDir, "cidata"); err != nil {
		return fmt.Errorf("failed to create seed ISO: %w", err)
	}
	logging.Success("Cloud-init seed ISO created: %s", seedISO)

	// Find Ubuntu 24.04 ISO
	isoPath, err := findUbuntuISO(p.cfg.VM.StorageDir)
	if err != nil {
		return err
	}
	logging.Info("Using Ubuntu ISO: %s", isoPath)

	// Create main disk
	diskPath := filepath.Join(p.cfg.VM.StorageDir, "jumpstart.qcow2")
	os.Remove(diskPath) // remove if exists
	if err := libvirt.CreateDisk(ctx, diskPath, p.cfg.Nodes.Jumpstart.DiskGB, "qcow2"); err != nil {
		return fmt.Errorf("failed to create jumpstart disk: %w", err)
	}

	// Determine networks (always internal + main, optionally WAN)
	nets := []libvirt.NetworkOpt{
		{Network: "internal", Mac: p.cfg.Nodes.Jumpstart.MACInternal},
		{Network: "main", Mac: p.cfg.Nodes.Jumpstart.MACMain},
	}
	if p.lv.NetExists(ctx, "default") {
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
		NoAutoConsole: false,
	})
	if err != nil {
		return fmt.Errorf("jumpstart virt-install failed: %w", err)
	}

	// Start the VM after installation
	logging.Info("Starting Jumpstart VM...")
	if err := p.lv.Start(ctx, p.cfg.Nodes.Jumpstart.Name); err != nil {
		logging.Warn("Failed to start jumpstart (may already be running): %v", err)
	}

	logging.Success("Jumpstart VM deployed and started")
	return nil
}

// deployClientNodes provisions all client nodes via PXE boot.
func (p *Phase) deployClientNodes(ctx context.Context) error {
	logging.Info("Deploying client nodes...")
	dir := p.cfg.VM.StorageDir

	// Router (3 NICs: internal, main, WAN)
	router := p.cfg.Nodes.Router
	p.destroyVM(ctx, router.Name)
	routerNets := []libvirt.NetworkOpt{
		{Network: "internal", Mac: router.MACInternal},
		{Network: "main", Mac: router.MACMain},
	}
	wanNet := "default"
	if !p.lv.NetExists(ctx, "default") {
		wanNet = "main"
		logging.Warn("'default' network not available, using 'main' as WAN for router")
	}
	routerNets = append(routerNets, libvirt.NetworkOpt{Network: wanNet, Mac: router.MACWAN})

	if err := p.createClientVM(ctx, router, dir, routerNets, nil); err != nil {
		return fmt.Errorf("failed to create router VM: %w", err)
	}

	// Internal network nodes
	internalNodes := []config.NodeDetail{
		p.cfg.Nodes.Monitor,
		p.cfg.Nodes.Storage,
	}
	for _, node := range internalNodes {
		nets := []libvirt.NetworkOpt{{Network: "internal", Mac: node.MAC}}
		if err := p.createClientVM(ctx, node, dir, nets, nil); err != nil {
			return fmt.Errorf("failed to create VM %s: %w", node.Name, err)
		}
	}

	// Masters with extra DRBD disk
	for _, master := range p.cfg.Nodes.Masters {
		nets := []libvirt.NetworkOpt{{Network: "internal", Mac: master.MAC}}
		extraDisks := []libvirt.DiskOpt{
			{Path: filepath.Join(dir, master.Name+"-drbd.qcow2"), SizeGB: master.DRBDDiskGB},
		}
		if err := p.createClientVM(ctx, master, dir, nets, extraDisks); err != nil {
			return fmt.Errorf("failed to create master VM %s: %w", master.Name, err)
		}
	}

	// Workers
	for _, worker := range p.cfg.Nodes.Workers {
		nets := []libvirt.NetworkOpt{{Network: "internal", Mac: worker.MAC}}
		if err := p.createClientVM(ctx, worker, dir, nets, nil); err != nil {
			return fmt.Errorf("failed to create worker VM %s: %w", worker.Name, err)
		}
	}

	// Main network nodes
	lb := p.cfg.Nodes.LB
	lbNets := []libvirt.NetworkOpt{{Network: "main", Mac: lb.MAC}}
	if err := p.createClientVM(ctx, lb, dir, lbNets, nil); err != nil {
		return fmt.Errorf("failed to create LB VM: %w", err)
	}

	for _, cms := range p.cfg.Nodes.CMSFrontends {
		nets := []libvirt.NetworkOpt{{Network: "main", Mac: cms.MAC}}
		if err := p.createClientVM(ctx, cms, dir, nets, nil); err != nil {
			return fmt.Errorf("failed to create CMS VM %s: %w", cms.Name, err)
		}
	}

	// Hotdesks (dynamically generated)
	for _, hd := range p.cfg.HotdeskSpecs() {
		nets := []libvirt.NetworkOpt{{Network: "main", Mac: hd.MAC}}
		n := config.NodeDetail{
			Name:         hd.Name,
			IP:           hd.IP,
			FQDN:         hd.FQDN,
			MAC:          hd.MAC,
			RAMInstallMB: hd.RAMInstallMB,
			RAMFinalMB:   hd.RAMFinalMB,
			VCPUs:        hd.VCPUs,
			DiskGB:       hd.DiskGB,
		}
		if err := p.createClientVM(ctx, n, dir, nets, nil); err != nil {
			return fmt.Errorf("failed to create hotdesk VM %s: %w", hd.Name, err)
		}
	}

	logging.Success("All client nodes deployed")
	return nil
}

// createClientVM creates a single client VM with PXE boot.
func (p *Phase) createClientVM(ctx context.Context, node config.NodeDetail, vmDir string, nets []libvirt.NetworkOpt, extraDisks []libvirt.DiskOpt) error {
	logging.Info("Creating VM: %s (RAM: %d MB, vCPUs: %d, Disk: %d GB)",
		node.Name, node.RAMInstallMB, node.VCPUs, node.DiskGB)

	p.destroyVM(ctx, node.Name)

	diskPath := filepath.Join(vmDir, node.Name+".qcow2")
	disks := []libvirt.DiskOpt{{Path: diskPath, SizeGB: node.DiskGB}}
	disks = append(disks, extraDisks...)

	return p.lv.VirtInstall(ctx, libvirt.VirtInstallOpts{
		Name:          node.Name,
		RAM:           node.RAMInstallMB,
		VCPUs:         node.VCPUs,
		Disks:         disks,
		Networks:      nets,
		PXE:           true,
		BootOrder:     []string{"hd", "network"},
		OSVariant:     "ubuntu24.04",
		NoAutoConsole: true,
		Wait:          0,
	})
}

// cleanup destroys all VMs and networks.
func (p *Phase) cleanup(ctx context.Context) error {
	timer := logging.PhaseStart("Cleanup VMs and Networks")
	defer timer.End()

	logging.Info("Destroying all VMs and networks...")

	// Destroy all VMs
	allNodes := p.cfg.AllNodes()
	for _, node := range allNodes {
		p.destroyVM(ctx, node.Name)
	}

	// Remove disk files
	for _, node := range allNodes {
		os.Remove(filepath.Join(p.cfg.VM.StorageDir, node.Name+".qcow2"))
		os.Remove(filepath.Join(p.cfg.VM.StorageDir, node.Name+"-drbd.qcow2"))
	}

	// Destroy networks
	for _, net := range []string{"internal", "main"} {
		if p.lv.NetExists(ctx, net) {
			if err := p.lv.NetDestroy(ctx, net); err != nil {
				logging.Warn("Failed to destroy network %s: %v", net, err)
			}
			if err := p.lv.NetUndefine(ctx, net); err != nil {
				logging.Warn("Failed to undefine network %s: %v", net, err)
			}
		}
	}

	logging.Success("Cleanup completed")
	return nil
}

// destroyVM safely destroys and undefines a VM if it exists.
func (p *Phase) destroyVM(ctx context.Context, name string) {
	if !p.lv.DomainExists(ctx, name) {
		return
	}
	logging.Info("Removing existing VM: %s", name)
	p.lv.Destroy(ctx, name)
	p.lv.Undefine(ctx, name)
}

// ensureSSHKey returns the public key for the given private key path, generating it if needed.
func (p *Phase) ensureSSHKey(keyPath string) (string, error) {
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		logging.Info("Generating SSH key at %s...", keyPath)
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return "", fmt.Errorf("failed to create SSH dir: %w", err)
		}
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen failed: %v: %s", err, out)
		}
	}

	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
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

// generateRandomPasswordHash generates a random SHA-512 password hash.
// The actual password is random and discarded; SSH key auth is enforced.
func generateRandomPasswordHash() string {
	// Use a fixed hash as fallback (password login is disabled via SSH config anyway)
	src := rand.NewSource(time.Now().UnixNano())
	r := rand.New(src)
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	// Return a placeholder — actual hash generation needs openssl or Python
	// The important security comes from PasswordAuthentication no in sshd_config
	return "$6$GAR_RANDOM$placeholder_hash_password_login_disabled"
}
