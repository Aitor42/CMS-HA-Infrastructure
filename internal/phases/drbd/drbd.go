package drbd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases/catalog"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

// replicationWait is a variable so tests can exercise the failure path without
// waiting five minutes. Production keeps the bounded retry budget.
var replicationWait = retry.Config{MaxAttempts: 30, Interval: 5 * time.Second, Timeout: 5 * time.Minute}

// Phase implements the DRBD setup phase.
type Phase struct {
	cfg  *config.Config
	pool ssh.Executor
}

// NewPhase creates a new DRBD Phase.
func NewPhase(cfg *config.Config, pool ssh.Executor) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

// Description returns the phase description.
func (p *Phase) Description() string { return catalog.DRBD.Description }

// Name returns the phase name.
func (p *Phase) Name() string { return catalog.DRBD.Name }

// Run configures DRBD, fencing and the promote-on-boot guard, then verifies
// that both peers are connected with up-to-date disks.
func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	if len(p.cfg.Nodes.Masters) < 2 {
		return fmt.Errorf("DRBD requires at least 2 master nodes")
	}

	master1 := p.cfg.Nodes.Masters[0]
	master2 := p.cfg.Nodes.Masters[1]
	drbdPair := []config.NodeDetail{master1, master2}

	if err := p.setupFencing(ctx, drbdPair); err != nil {
		return err
	}
	if err := p.installDRBD(ctx, drbdPair); err != nil {
		return err
	}
	if err := p.deployDRBDConfig(ctx, drbdPair); err != nil {
		return err
	}
	if err := p.initDRBDMetadata(ctx, drbdPair); err != nil {
		return err
	}
	primary, err := p.resolvePrimary(ctx, drbdPair)
	if err != nil {
		return err
	}
	if err := p.waitForReplication(ctx, primary); err != nil {
		return err
	}
	if err := p.formatAndMount(ctx, primary); err != nil {
		return err
	}
	p.updateFstab(ctx, drbdPair)
	// Install the boot guard before the watchdog unit is enabled: the watchdog
	// has Wants=drbd-cms-boot.service, and starting it on a first deploy must
	// not race a missing unit. The guard is idempotent and runs again at boot.
	if err := p.deployBootRoleResolution(ctx, master1, master2); err != nil {
		return err
	}
	if err := p.deployFailoverScripts(ctx, drbdPair); err != nil {
		return err
	}
	p.deployTmpfiles(ctx, drbdPair)

	logging.Success("DRBD Setup completed successfully.")
	return nil
}

// installDRBD installs drbd-utils and the kernel module on both peers in
// parallel. The PXE install pulls linux-modules-extra in as part of the
// server seed, but the cloud image does not, so drbd.ko is absent there and
// `modprobe drbd` fails during metadata initialization.
func (p *Phase) installDRBD(ctx context.Context, pair []config.NodeDetail) error {
	logging.Info("Installing DRBD utils and kernel module on master nodes...")
	installCmd := ssh.AptLockWaitSnippet + `
apt-get update
# The PXE seed already ships linux-modules-extra, and older kernels may no
# longer have that exact package in the archive. Only install it when the
# running kernel really lacks drbd.ko; the cloud image is the case that needs
# it.
if ! modinfo drbd >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y "linux-modules-extra-$(uname -r)"
fi
DEBIAN_FRONTEND=noninteractive apt-get install -y drbd-utils
`

	for _, r := range p.pool.RunParallel(ctx, []string{pair[0].IP, pair[1].IP}, installCmd) {
		if r.Err != nil {
			return fmt.Errorf("failed to install DRBD on %s: %w", r.Host, r.Err)
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("failed to install DRBD on %s (exit %d): %s", r.Host, r.ExitCode, strings.TrimSpace(r.Output))
		}
	}
	return nil
}

// deployDRBDConfig renders the resource template and copies it to both peers.
func (p *Phase) deployDRBDConfig(ctx context.Context, pair []config.NodeDetail) error {
	logging.Info("Deploying DRBD configuration...")
	tmplContent, err := cms.TemplatesFS.ReadFile("templates/drbd/cms-data.res")
	if err != nil {
		return fmt.Errorf("failed to read template: %w", err)
	}

	tmpl, err := template.New("drbd").Parse(string(tmplContent))
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p.cfg); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	drbdCfgBytes := buf.Bytes()
	tasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)
	for _, ip := range []string{pair[0].IP, pair[1].IP} {
		nodeIP := ip
		tasks[nodeIP] = func(c context.Context, pool ssh.Executor) error {
			return pool.CopyContent(c, nodeIP, drbdCfgBytes, "/etc/drbd.d/cms_data.res", 0644)
		}
	}
	for _, r := range p.pool.RunParallelFunc(ctx, tasks) {
		if r.Err != nil {
			return fmt.Errorf("failed to copy DRBD config to %s: %w", r.Host, r.Err)
		}
	}
	return nil
}

// initDRBDMetadata creates the metadata (idempotently) and brings the resource
// up on both peers in parallel.
func (p *Phase) initDRBDMetadata(ctx context.Context, pair []config.NodeDetail) error {
	logging.Info("Initializing DRBD metadata in parallel...")
	// create-md fails when metadata already exists (expected on re-runs): that
	// is confirmed with dump-md, and `set -e` surfaces every other error.
	// Do NOT pass --force here: on a re-run it would overwrite the metadata of
	// both peers, discarding the replication UUID/history and potentially
	// forcing a full resync (or, worse, making two stale disks look fresh).
	// The script runs through RunScript inside RunParallelFunc so drbdadm's
	// stderr (which reports the expected "already exists" condition) is kept;
	// plain RunParallel only surfaces stdout.
	initScript := `set -e
modprobe drbd
if ! drbdadm create-md cms_data; then
    drbdadm dump-md cms_data >/dev/null 2>&1 || {
        echo "drbdadm create-md failed and no existing metadata was found" >&2
        exit 1
    }
fi
drbdadm up cms_data
`
	initTasks := make(map[string]func(ctx context.Context, pool ssh.Executor) error)
	for _, ip := range []string{pair[0].IP, pair[1].IP} {
		nodeIP := ip
		initTasks[nodeIP] = func(c context.Context, pool ssh.Executor) error {
			_, err := pool.RunScript(c, nodeIP, initScript)
			return err
		}
	}
	for _, r := range p.pool.RunParallelFunc(ctx, initTasks) {
		if r.Err != nil {
			return fmt.Errorf("failed to initialize DRBD metadata on %s: %w", r.Host, r.Err)
		}
	}
	return nil
}

// resolvePrimary returns the node that already owns the Primary role, or
// promotes one safely when neither peer does. A resumed deploy after a
// failover must not force master1 to Primary while master2 is still the
// writer: that is a split brain. The same applies to a cleanly stopped pair
// whose disks are both UpToDate; promoting with --force there is unnecessary
// and discards DRBD's consistency checks.
func (p *Phase) resolvePrimary(ctx context.Context, pair []config.NodeDetail) (config.NodeDetail, error) {
	type peerState struct {
		node config.NodeDetail
		role string
		disk string
	}

	states := make([]peerState, 0, len(pair))
	var primaries []peerState
	for _, node := range pair {
		roleOut, roleErr, code, err := p.pool.RunCommand(ctx, node.IP, "drbdadm role cms_data")
		if err != nil || code != 0 {
			return config.NodeDetail{}, fmt.Errorf("read DRBD role on %s (exit %d): %v %s", node.Name, code, err, strings.TrimSpace(roleErr))
		}
		role := strings.TrimSpace(strings.SplitN(roleOut, "/", 2)[0])

		diskOut, _, diskCode, diskErr := p.pool.RunCommand(ctx, node.IP, "drbdadm dstate cms_data")
		disk := ""
		if diskErr == nil && diskCode == 0 {
			disk = strings.TrimSpace(strings.SplitN(diskOut, "/", 2)[0])
		}

		state := peerState{node: node, role: role, disk: disk}
		states = append(states, state)
		if role == "Primary" {
			primaries = append(primaries, state)
		}
	}

	switch len(primaries) {
	case 1:
		// An existing Primary is authoritative, but only if its local disk is
		// healthy. A Primary with an Inconsistent/Outdated disk would be
		// mounted below and served as if it were the latest copy.
		if primaries[0].disk != "" && primaries[0].disk != "UpToDate" {
			return config.NodeDetail{}, fmt.Errorf("refusing to use existing DRBD Primary %s: local disk state is %s, not UpToDate", primaries[0].node.Name, primaries[0].disk)
		}
		logging.Info("%s already holds the DRBD Primary role; leaving the role untouched.", primaries[0].node.Name)
		return primaries[0].node, nil
	case 2:
		names := make([]string, 0, len(primaries))
		for _, state := range primaries {
			names = append(names, state.node.Name)
		}
		return config.NodeDetail{}, fmt.Errorf("refusing to promote: both DRBD peers report Primary (%s); resolve the split brain first", strings.Join(names, ", "))
	}

	// Neither peer is Primary. Prefer a disk that is already UpToDate so the
	// promotion does not need --force; if neither is (a brand-new pair), fall
	// back to master1 with --force to seed the initial sync.
	chosen := states[0]
	for _, state := range states {
		if state.disk == "UpToDate" {
			chosen = state
			break
		}
	}

	promoteCmd := "drbdadm primary cms_data"
	if chosen.disk != "UpToDate" {
		logging.Warn("No DRBD peer reports an UpToDate disk; forcing the initial promotion on %s.", chosen.node.Name)
		promoteCmd += " --force"
	}

	logging.Info("Promoting %s to DRBD Primary...", chosen.node.Name)
	if _, _, code, err := p.pool.RunCommand(ctx, chosen.node.IP, promoteCmd); err != nil || code != 0 {
		return config.NodeDetail{}, fmt.Errorf("failed to promote %s to DRBD Primary: %v (exit code %d)", chosen.node.Name, err, code)
	}

	roleOut, roleErr, code, err := p.pool.RunCommand(ctx, chosen.node.IP, "drbdadm role cms_data")
	if err != nil || code != 0 || !strings.HasPrefix(strings.TrimSpace(roleOut), "Primary") {
		return config.NodeDetail{}, fmt.Errorf("promotion command succeeded but %s does not report Primary (exit %d): %v %s", chosen.node.Name, code, err, strings.TrimSpace(roleErr))
	}
	return chosen.node, nil
}

// waitForReplication waits until the peer is connected and its disk is
// UpToDate (or at least syncing). A connection alone is not enough. This is a
// gate: a deploy that cannot establish replication must not be reported as
// successful, because the next failover would have no replica to promote.
func (p *Phase) waitForReplication(ctx context.Context, primary config.NodeDetail) error {
	logging.Info("Waiting for DRBD replication to start/sync...")
	err := retry.DoContext(ctx, replicationWait, func(attemptCtx context.Context) error {
		out, errOut, code, err := p.pool.RunCommand(attemptCtx, primary.IP, "drbdadm status cms_data")
		if err != nil || code != 0 {
			return fmt.Errorf("drbdadm status failed (exit %d): %v %s", code, err, strings.TrimSpace(errOut))
		}
		// "Established"/"Connected" is only the connection state: the peer
		// disk must also be UpToDate (or at least syncing) before the
		// replica can be considered ready. A connection alone is not.
		connected := strings.Contains(out, "Established") || strings.Contains(out, "Connected")
		localReady := strings.Contains(out, "disk:UpToDate")
		diskReady := strings.Contains(out, "peer-disk:UpToDate") || strings.Contains(out, "ds:UpToDate") || strings.Contains(out, "SyncTarget")
		if !strings.Contains(out, "role:Primary") || !localReady || !connected || !diskReady {
			return fmt.Errorf("replication not ready: %s", strings.TrimSpace(out))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("DRBD replication did not become ready on %s: %w", primary.Name, err)
	}
	return nil
}

// formatAndMount initialises the filesystem once and mounts it on the current
// DRBD Primary.
func (p *Phase) formatAndMount(ctx context.Context, primary config.NodeDetail) error {
	logging.Info("Formatting and mounting DRBD volume on %s...", primary.Name)
	mountCmd := `
mkdir -p /mnt/data/mariadb
if ! blkid /dev/drbd0 >/dev/null 2>&1; then
    mkfs.ext4 -F /dev/drbd0
fi
if ! mountpoint -q /mnt/data/mariadb; then
    mount /dev/drbd0 /mnt/data/mariadb
fi
`
	if _, err := p.pool.RunScript(ctx, primary.IP, mountCmd); err != nil {
		return fmt.Errorf("failed to format and mount DRBD volume on %s: %w", primary.Name, err)
	}
	return nil
}

// updateFstab records the noauto mount on both peers. A failure is logged but
// not fatal: the volume is already mounted on the current Primary and the boot
// unit only needs the directory to exist.
func (p *Phase) updateFstab(ctx context.Context, pair []config.NodeDetail) {
	logging.Info("Updating fstab and mount directory on masters...")
	fstabCmd := "mkdir -p /mnt/data/mariadb && (grep -q '/dev/drbd0' /etc/fstab || echo '/dev/drbd0 /mnt/data/mariadb ext4 defaults,noauto 0 0' >> /etc/fstab)"
	for _, ip := range []string{pair[0].IP, pair[1].IP} {
		if _, _, _, err := p.pool.RunCommand(ctx, ip, fstabCmd); err != nil {
			logging.Warn("failed to update fstab on %s: %v", ip, err)
		}
	}
}

// deployFailoverScripts installs the failover script, the watchdog and its
// unit, then enables both services.
func (p *Phase) deployFailoverScripts(ctx context.Context, pair []config.NodeDetail) error {
	logging.Info("Deploying DRBD failover script and watchdog daemon...")
	scriptContent, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-failover.sh")
	if err != nil {
		return fmt.Errorf("failed to read drbd-failover.sh template: %w", err)
	}
	watchdogContent, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-watchdog.sh")
	if err != nil {
		return fmt.Errorf("failed to read drbd-watchdog.sh template: %w", err)
	}
	watchdogServiceContent, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-failover-watchdog.service")
	if err != nil {
		return fmt.Errorf("failed to read drbd-failover-watchdog.service template: %w", err)
	}

	for _, ip := range []string{pair[0].IP, pair[1].IP} {
		if err := p.pool.CopyContent(ctx, ip, []byte(scriptContent), "/usr/local/bin/drbd-failover.sh", 0755); err != nil {
			return fmt.Errorf("failed to deploy failover script to %s: %w", ip, err)
		}
		if err := p.pool.CopyContent(ctx, ip, []byte(watchdogContent), "/usr/local/bin/drbd-watchdog.sh", 0755); err != nil {
			return fmt.Errorf("failed to deploy watchdog script to %s: %w", ip, err)
		}
		if err := p.pool.CopyContent(ctx, ip, []byte(watchdogServiceContent), "/etc/systemd/system/drbd-failover-watchdog.service", 0644); err != nil {
			return fmt.Errorf("failed to deploy watchdog service to %s: %w", ip, err)
		}
		if _, _, _, err := p.pool.RunCommand(ctx, ip, "systemctl daemon-reload && systemctl enable --now drbd-failover-watchdog.service && systemctl enable drbd.service"); err != nil {
			return fmt.Errorf("failed to enable DRBD services on %s: %w", ip, err)
		}
	}
	return nil
}

// deployBootRoleResolution installs the boot-time role resolver. This unit is
// the only thing that promotes on boot: it resolves a stale Primary (power
// cut, hypervisor crash, fence) before k3s starts, so a node that restarts
// while its peer is primary comes back as Secondary instead of becoming a
// second writer. Master 1 is the designated primary; master 2 only resolves
// its role and never promotes itself.
func (p *Phase) deployBootRoleResolution(ctx context.Context, master1, master2 config.NodeDetail) error {
	logging.Info("Deploying DRBD boot-time role resolution on both masters...")
	bootScript, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-boot.sh")
	if err != nil {
		return fmt.Errorf("failed to read drbd-boot.sh template: %w", err)
	}
	for _, m := range []struct {
		node    config.NodeDetail
		promote int
	}{{master1, 1}, {master2, 0}} {
		if err := p.pool.CopyContent(ctx, m.node.IP, bootScript, "/usr/local/sbin/drbd-boot.sh", 0755); err != nil {
			return fmt.Errorf("failed to deploy drbd-boot.sh to %s: %w", m.node.IP, err)
		}
		unit := fmt.Sprintf(`[Unit]
Description=Resolve the DRBD cms_data role before k3s starts
After=drbd.service network-online.target
Wants=drbd.service network-online.target
Before=k3s.service

[Service]
Type=oneshot
RemainAfterExit=yes
Environment=PROMOTE=%d
ExecStart=/usr/local/sbin/drbd-boot.sh

[Install]
WantedBy=multi-user.target
`, m.promote)
		if err := p.pool.CopyContent(ctx, m.node.IP, []byte(unit), "/etc/systemd/system/drbd-cms-boot.service", 0644); err != nil {
			return fmt.Errorf("failed to deploy drbd-cms-boot.service to %s: %w", m.node.IP, err)
		}
		// Retire the old promote-only unit: if both were enabled the old one
		// could promote before the stale-Primary guard had run.
		cmd := "systemctl disable --now drbd-cms-primary.service 2>/dev/null; " +
			"rm -f /etc/systemd/system/drbd-cms-primary.service; " +
			"systemctl daemon-reload && systemctl enable drbd-cms-boot.service"
		if _, _, _, err := p.pool.RunCommand(ctx, m.node.IP, cmd); err != nil {
			return fmt.Errorf("failed to enable drbd-cms-boot.service on %s: %w", m.node.IP, err)
		}
	}
	return nil
}

// deployTmpfiles creates the MariaDB data directories at boot with the right
// ownership. Failures are warnings, as before: the mount script already
// creates the directory and MariaDB sets its own ownership.
func (p *Phase) deployTmpfiles(ctx context.Context, pair []config.NodeDetail) {
	tmpfilesContent := "d /mnt/data 0755 root root -\nd /mnt/data/mariadb 0755 999 999 -\n"
	for _, ip := range []string{pair[0].IP, pair[1].IP} {
		if err := p.pool.CopyContent(ctx, ip, []byte(tmpfilesContent), "/etc/tmpfiles.d/mariadb-datadir.conf", 0644); err != nil {
			logging.Warn("Failed to upload mariadb-datadir.conf on %s: %v", ip, err)
		} else {
			p.pool.RunCommand(ctx, ip, "systemd-tmpfiles --create /etc/tmpfiles.d/mariadb-datadir.conf 2>/dev/null || true")
		}
	}
}

// setupFencing provisions STONITH fencing for the 2-node DRBD resource: it
// deploys fence-peer.sh and a per-node /etc/drbd-fence.conf to the pair, and
// (when no custom command is configured) creates an SSH key on the hypervisor,
// authorises it for the user running the orchestrator (the same $USER the fence
// command SSHes as) and copies the private key to the masters so they can drive
// libvirt on the hypervisor.
func (p *Phase) setupFencing(ctx context.Context, pair []config.NodeDetail) error {
	fenceRequired := "0"
	if p.cfg.DRBD.Fencing {
		fenceRequired = "1"
	}
	logging.Info("Configuring DRBD STONITH fencing (required=%s)...", fenceRequired)

	offCmd, stateCmd := p.cfg.FenceCommands()
	custom := p.cfg.DRBD.FenceCommand != ""

	fenceScript, confTmpl, err := p.loadFenceTemplates()
	if err != nil {
		return err
	}

	// Derived (SSH) fencing needs a key on the hypervisor and a copy on the
	// masters. This is fatal on purpose when fencing is required: the watchdog
	// refuses to promote a node that cannot fence its peer, so a "successful"
	// deploy with no key would silently ship a cluster without automatic
	// failover — and only reveal it at the end of the chaos test.
	var fenceKey []byte
	if p.cfg.DRBD.Fencing && !custom {
		fenceKey, err = p.ensureFenceKey()
		if err != nil {
			return fmt.Errorf("could not provision the hypervisor fence SSH key, so automatic promotion would never fence: %w "+
				"(set drbd.fencing: false to accept that, or configure a custom drbd.fence_command)", err)
		}
	}

	if err := p.deployFenceArtifacts(ctx, pair, fenceScript, confTmpl, fenceKey, fenceRequired, offCmd, stateCmd); err != nil {
		return err
	}

	logging.Success("DRBD fencing configured on %s and %s.", pair[0].Name, pair[1].Name)
	return nil
}

func (p *Phase) loadFenceTemplates() ([]byte, *template.Template, error) {
	fenceScript, err := cms.TemplatesFS.ReadFile("templates/drbd/fence-peer.sh")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read fence-peer.sh: %w", err)
	}
	confContent, err := cms.TemplatesFS.ReadFile("templates/drbd/drbd-fence.conf")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read drbd-fence.conf template: %w", err)
	}
	confTmpl, err := template.New("drbd-fence").Parse(string(confContent))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse drbd-fence.conf template: %w", err)
	}
	return fenceScript, confTmpl, nil
}

func (p *Phase) deployFenceArtifacts(ctx context.Context, pair []config.NodeDetail, fenceScript []byte, confTmpl *template.Template, fenceKey []byte, fenceRequired, offCmd, stateCmd string) error {
	for i, node := range pair {
		peer := pair[(i+1)%len(pair)] // DRBD cms_data is a 2-node resource

		if _, _, _, err := p.pool.RunCommand(ctx, node.IP, "mkdir -p /etc/drbd-fence && chmod 700 /etc/drbd-fence"); err != nil {
			return fmt.Errorf("failed to prepare /etc/drbd-fence on %s: %w", node.Name, err)
		}

		if err := p.pool.CopyContent(ctx, node.IP, fenceScript, "/usr/local/bin/fence-peer.sh", 0755); err != nil {
			return fmt.Errorf("failed to deploy fence-peer.sh to %s: %w", node.Name, err)
		}

		data := struct {
			FenceRequired     string
			PeerDomain        string
			FenceCommand      string
			FenceStateCommand string
		}{fenceRequired, peer.Name, offCmd, stateCmd}

		var buf bytes.Buffer
		if err := confTmpl.Execute(&buf, data); err != nil {
			return fmt.Errorf("failed to render drbd-fence.conf for %s: %w", node.Name, err)
		}

		if len(fenceKey) > 0 {
			if err := p.pool.CopyContent(ctx, node.IP, fenceKey, "/etc/drbd-fence/fence_key", 0600); err != nil {
				return fmt.Errorf("failed to copy fence key to %s: %w", node.Name, err)
			}
		}
		if err := p.pool.CopyContent(ctx, node.IP, buf.Bytes(), "/etc/drbd-fence.conf", 0600); err != nil {
			return fmt.Errorf("failed to deploy drbd-fence.conf to %s: %w", node.Name, err)
		}
	}
	return nil
}

// ensureFenceKey creates (if needed) a dedicated ed25519 key on the hypervisor,
// authorises it for the current user and returns the private key material.
func (p *Phase) ensureFenceKey() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(home, ".ssh")
	keyPath := filepath.Join(sshDir, "id_ed25519_fence")

	if _, err := os.Stat(keyPath); err != nil {
		if err := os.MkdirAll(sshDir, 0700); err != nil {
			return nil, err
		}
		out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", "cms-ha-fence", "-f", keyPath).CombinedOutput() // #nosec G204
		if err != nil {
			return nil, fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, err
	}
	authPath := filepath.Join(sshDir, "authorized_keys")
	existing, _ := os.ReadFile(authPath)
	if !strings.Contains(string(existing), strings.TrimSpace(string(pub))) {
		f, err := os.OpenFile(authPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) // #nosec G304
		if err != nil {
			return nil, err
		}
		_, werr := f.Write(append(pub, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}
	}

	return os.ReadFile(keyPath)
}
