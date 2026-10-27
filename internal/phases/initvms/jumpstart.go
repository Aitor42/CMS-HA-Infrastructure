package initvms

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/hypervisor"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/libvirt"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
)

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
	// WANIfaceConfigCloud is the same optional WAN stanza for the standalone
	// NoCloud network-config file, where ethernets sits at the document root.
	WANIfaceConfigCloud string
}

// cloudNodeData is the cloud-init user-data for a client node created through
// the fast path. Interfaces is built from the NICs the topology assigns to the
// node, so internal, main and router nodes all share one template.
type cloudNodeData struct {
	Hostname     string
	PasswordHash string
	PubKey       string
	Interfaces   []cloudInterface
}

// cloudInterface is one cloud-init ethernet stanza. DHCP is used for the WAN
// uplink; every lab-facing NIC gets a fixed address so the node is reachable
// before any DHCP server exists.
type cloudInterface struct {
	Name    string
	MAC     string
	IP      string
	Gateway string
	DNS     string
	DHCP    bool
	Metric  int
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

	mode, err := p.jumpstartImageMode()
	if err != nil {
		return err
	}
	logging.Info("Jumpstart install path: %s", mode)

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

	// Determine WAN interface config (if libvirt default network exists). The
	// two indentation levels match the autoinstall and cloud-init templates.
	hasDefaultNet := p.lv.NetExists(ctx, "default")
	wanIfaceConfig := ""
	wanIfaceConfigCloud := ""
	if hasDefaultNet {
		wanIfaceConfig = fmt.Sprintf("\n      net2:\n        match:\n          macaddress: \"%s\"\n        dhcp4: true\n        dhcp4-overrides:\n          route-metric: 100\n          use-dns: true", p.cfg.Nodes.Jumpstart.MACWAN)
		wanIfaceConfigCloud = fmt.Sprintf("\n  net2:\n    match:\n      macaddress: \"%s\"\n    dhcp4: true\n    dhcp4-overrides:\n      route-metric: 100\n      use-dns: true", p.cfg.Nodes.Jumpstart.MACWAN)
	}

	data := autoinstallData{
		Hostname:            p.cfg.Nodes.Jumpstart.Name,
		PasswordHash:        passHash,
		PubKey:              pubKey,
		MACInternal:         p.cfg.Nodes.Jumpstart.MACInternal,
		IPInternal:          p.cfg.Nodes.Jumpstart.IP,
		GatewayInternal:     p.cfg.Network.Internal.Gateway,
		MACMain:             p.cfg.Nodes.Jumpstart.MACMain,
		IPMain:              p.cfg.Nodes.Jumpstart.IPMain,
		WANIfaceConfig:      wanIfaceConfig,
		WANIfaceConfigCloud: wanIfaceConfigCloud,
	}

	// Resolve the install artifact before writing the seed ISO, so a missing
	// ISO/cloud image fails without leaving a half-prepared seed behind.
	var baseImage, isoPath string
	switch mode {
	case "cloud":
		baseImage, err = findCloudImage(p.cfg.VM.StorageDir, p.cfg.VM.JumpstartBaseImage)
		if err != nil {
			return err
		}
		logging.Info("Using Ubuntu cloud image: %s", baseImage)
	case "iso":
		isoPath, err = findUbuntuISO(p.cfg.VM.StorageDir)
		if err != nil {
			return err
		}
		logging.Info("Using Ubuntu ISO: %s", isoPath)
	}

	seedISO, err := p.createJumpstartSeed(ctx, mode, data)
	if err != nil {
		return err
	}

	diskPath := filepath.Join(p.cfg.VM.StorageDir, "jumpstart.qcow2")
	if err := hypervisor.EnsureDiskPathAvailable(diskPath); err != nil {
		return err
	}

	// Determine networks (always internal + main, optionally WAN)
	nets := []libvirt.NetworkOpt{
		{Network: "internal", Mac: p.cfg.Nodes.Jumpstart.MACInternal},
		{Network: "main", Mac: p.cfg.Nodes.Jumpstart.MACMain},
	}
	if hasDefaultNet {
		nets = append(nets, libvirt.NetworkOpt{Network: "default", Mac: p.cfg.Nodes.Jumpstart.MACWAN})
	}

	// The ISO path needs the installer's extra RAM; the cloud image is already
	// installed and boots straight into the runtime size. Resizing it later is
	// not an option: `deploy` has no post-provision shrink step for the
	// jumpstart, so a cloud guest left at RAMInstallMB would keep that
	// footprint for the whole run.
	jumpstartRAM := p.cfg.Nodes.Jumpstart.RAMInstallMB
	if mode == "cloud" {
		jumpstartRAM = p.cfg.Nodes.Jumpstart.RuntimeRAMMB()
	}

	installOpts := libvirt.VirtInstallOpts{
		Name:          p.cfg.Nodes.Jumpstart.Name,
		RAM:           jumpstartRAM,
		VCPUs:         p.cfg.Nodes.Jumpstart.VCPUs,
		Networks:      nets,
		OSVariant:     "ubuntu24.04",
		GraphicsNone:  true,
		NoAutoConsole: true,
	}

	switch mode {
	case "cloud":
		if err := libvirt.CreateOverlayDisk(ctx, diskPath, baseImage, p.cfg.Nodes.Jumpstart.DiskGB); err != nil {
			return fmt.Errorf("failed to create jumpstart overlay disk: %w", err)
		}
		installOpts.Disks = []libvirt.DiskOpt{
			{Path: diskPath},
			{Path: seedISO},
		}
		installOpts.Import = true
		installOpts.BootOrder = []string{"hd"}
		installOpts.NoWait = true
	case "iso":
		if err := libvirt.CreateDisk(ctx, diskPath, p.cfg.Nodes.Jumpstart.DiskGB, "qcow2"); err != nil {
			return fmt.Errorf("failed to create jumpstart disk: %w", err)
		}
		installOpts.Disks = []libvirt.DiskOpt{
			{Path: diskPath},
			{Path: isoPath},
			{Path: seedISO},
		}
		installOpts.Location = isoPath + ",kernel=casper/vmlinuz,initrd=casper/initrd"
		installOpts.ExtraArgs = "autoinstall ds=nocloud console=ttyS0,115200n8"
		installOpts.Wait = -1
		installOpts.NoReboot = true
	}
	if err := hypervisor.RestrictDisk(diskPath); err != nil {
		return fmt.Errorf("restrict jumpstart disk: %w", err)
	}

	if mode == "cloud" {
		logging.Info("Starting Jumpstart cloud boot (the image is already installed)...")
	} else {
		logging.Info("Starting Jumpstart automated installation (this may take several minutes)...")
	}
	if err := p.lv.VirtInstall(ctx, installOpts); err != nil {
		return fmt.Errorf("jumpstart virt-install failed: %w", err)
	}

	logging.Info("Starting Jumpstart VM...")
	// The ISO path leaves the guest powered off after Subiquity finishes;
	// --import boots it immediately, so only start it when needed.
	running := false
	if state, err := p.lv.DomainState(ctx, p.cfg.Nodes.Jumpstart.Name); err == nil && state == "running" {
		running = true
	}
	if !running {
		if err := p.lv.Start(ctx, p.cfg.Nodes.Jumpstart.Name); err != nil {
			logging.Warn("Failed to start jumpstart (may already be running): %v", err)
		}
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

// jumpstartImageMode resolves the install path: the CLI flag wins, then the
// config value, then the ISO default.
func (p *Phase) jumpstartImageMode() (string, error) {
	mode := strings.ToLower(strings.TrimSpace(p.opts.JumpstartImage))
	if mode == "" {
		mode = p.cfg.JumpstartImageMode()
	}
	switch mode {
	case "iso", "cloud":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid jumpstart image %q: want \"iso\" or \"cloud\"", mode)
	}
}

// createJumpstartSeed renders the mode-specific cloud-init user-data and packs
// it into the NoCloud seed ISO shared by both install paths.
func (p *Phase) createJumpstartSeed(ctx context.Context, mode string, data autoinstallData) (string, error) {
	templatePath := "templates/autoinstall/user-data.tmpl"
	seedDirName := "autoinstall"
	metaData := ""
	if mode == "cloud" {
		templatePath = "templates/autoinstall/cloud-user-data.tmpl"
		seedDirName = "cloud-init"
		// A stable instance-id would make cloud-init skip its modules on a
		// reused disk; every jumpstart disk is new, so a fresh id is safest.
		metaData = fmt.Sprintf("instance-id: cms-ha-jumpstart-%d\nlocal-hostname: %s\n", time.Now().Unix(), data.Hostname)
	}

	userData, err := renderTemplate(templatePath, data)
	if err != nil {
		return "", fmt.Errorf("render %s: %w", templatePath, err)
	}

	var networkConfig []byte
	if mode == "cloud" {
		networkConfig, err = renderTemplate("templates/autoinstall/cloud-network-config.tmpl", data)
		if err != nil {
			return "", fmt.Errorf("render cloud network-config: %w", err)
		}
	}

	seedDir := filepath.Join(p.cfg.VM.StorageDir, seedDirName)
	seedISO := filepath.Join(p.cfg.VM.StorageDir, "seed.iso")
	if err := p.writeSeed(ctx, seedDir, seedISO, metaData, userData, networkConfig); err != nil {
		return "", err
	}
	return seedISO, nil
}

// createCloudNodeSeed renders the per-node cloud-init user-data and packs it in
// its own seed ISO. A shared seed would give every VM the same hostname and
// address, so the fast path pays one tiny ISO per node.
func (p *Phase) createCloudNodeSeed(ctx context.Context, name string, data cloudNodeData) (string, error) {
	userData, err := renderTemplate("templates/autoinstall/cloud-node-user-data.tmpl", data)
	if err != nil {
		return "", fmt.Errorf("render cloud-node user-data: %w", err)
	}
	networkConfig, err := renderTemplate("templates/autoinstall/cloud-node-network-config.tmpl", data)
	if err != nil {
		return "", fmt.Errorf("render cloud node network-config: %w", err)
	}
	metaData := fmt.Sprintf("instance-id: cms-ha-%s-%d\nlocal-hostname: %s\n", name, time.Now().UnixNano(), data.Hostname)
	seedDir := filepath.Join(p.cfg.VM.StorageDir, "cloud-init-"+name)
	seedISO := filepath.Join(p.cfg.VM.StorageDir, name+"-seed.iso")
	if err := p.writeSeed(ctx, seedDir, seedISO, metaData, userData, networkConfig); err != nil {
		return "", err
	}
	return seedISO, nil
}

// writeSeed writes the NoCloud files and turns them into a cidata ISO. The
// network-config file is separate from user-data: cloud-init explicitly does
// not read network configuration from user-data, and a missing file would make
// the cloud image fall back to DHCP instead of the topology's static address.
func (p *Phase) writeSeed(ctx context.Context, seedDir, seedISO, metaData string, userData, networkConfig []byte) error {
	if err := os.MkdirAll(seedDir, 0700); err != nil {
		return fmt.Errorf("failed to create %s dir: %w", filepath.Base(seedDir), err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), userData, 0600); err != nil {
		return fmt.Errorf("failed to write user-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(metaData), 0600); err != nil {
		return fmt.Errorf("failed to write meta-data: %w", err)
	}
	if len(networkConfig) > 0 {
		if err := os.WriteFile(filepath.Join(seedDir, "network-config"), networkConfig, 0600); err != nil {
			return fmt.Errorf("failed to write network-config: %w", err)
		}
	}
	if err := libvirt.CreateISO(ctx, seedISO, seedDir, "cidata"); err != nil {
		return fmt.Errorf("failed to create seed ISO: %w", err)
	}
	if err := hypervisor.RestrictDisk(seedISO); err != nil {
		return fmt.Errorf("restrict seed ISO: %w", err)
	}
	logging.Success("Cloud-init seed ISO created: %s", seedISO)
	return nil
}

// renderTemplate renders one embedded cloud-init template. It is separate from
// createJumpstartSeed so tests can assert the rendered YAML without xorrisofs.
func renderTemplate(path string, data any) ([]byte, error) {
	content, err := cms.TemplatesFS.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New(filepath.Base(path)).Parse(string(content))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// findCloudImage locates the Ubuntu cloud image for the opt-in cloud jumpstart.
// Unlike the ISO path it returns instructions instead of downloading: a base
// image is a 600 MB artifact and pinning it is the caller's decision.
func findCloudImage(dir, configured string) (string, error) {
	if configured != "" {
		path := config.ExpandPath(configured)
		if info, err := os.Stat(path); err == nil && info.Size() > 100<<20 {
			return path, nil
		}
		return "", fmt.Errorf("vm.jumpstart_base_image %s is missing or too small; expected the Ubuntu 24.04 cloud image", path)
	}

	candidates := []string{
		filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.img"),
		filepath.Join(dir, "ubuntu-24.04-server-cloudimg-amd64.qcow2"),
		filepath.Join(dir, "noble-server-cloudimg-amd64.img"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Size() > 100<<20 {
			return c, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*cloudimg*amd64*.img"))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Size() > 100<<20 {
			return m, nil
		}
	}

	return "", fmt.Errorf("no Ubuntu 24.04 cloud image found in %s; download noble-server-cloudimg-amd64.img from https://cloud-images.ubuntu.com/noble/current/ or set vm.jumpstart_base_image", dir)
}

// clientProvisioningMode resolves how client nodes are installed: the flag
// wins, then vm.provisioning, then the PXE default.
