package drbd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
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
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// replicationWait is a variable so tests can exercise the failure path without
// waiting five minutes. Production keeps the bounded retry budget.
var replicationWait = retry.Config{MaxAttempts: 30, Interval: 5 * time.Second, Timeout: 5 * time.Minute}

var (
	// The local role is the resource line ("cms_data role:Primary"); the peer
	// role appears on an indented "peer role:..." line and must not satisfy a
	// check that the local node is Primary.
	drbdLocalRolePattern = regexp.MustCompile(`(?m)^\S+\s+role:(Primary|Secondary|Unknown)\s*$`)
	// The local disk state is the "  disk:UpToDate" line. The peer's state is
	// reported as "peer-disk:..." and must not be mistaken for the local one:
	// a Primary with a stale local disk would otherwise pass this gate because
	// its healthy peer reports UpToDate.
	drbdLocalDiskPattern = regexp.MustCompile(`(?m)^[ \t]+disk:(\S+)\s*$`)
)

// drbdLocalRole returns the local role from `drbdadm status` output, or "" when
// the output does not contain a recognisable resource line.
func drbdLocalRole(status string) string {
	if m := drbdLocalRolePattern.FindStringSubmatch(status); len(m) == 2 {
		return m[1]
	}
	return ""
}

// drbdLocalDisk returns the local disk state from `drbdadm status` output, or
// "" when it cannot be read.
func drbdLocalDisk(status string) string {
	if m := drbdLocalDiskPattern.FindStringSubmatch(status); len(m) == 2 {
		return m[1]
	}
	return ""
}

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

		diskOut, diskErrOut, diskCode, diskErr := p.pool.RunCommand(ctx, node.IP, "drbdadm dstate cms_data")
		if diskErr != nil || diskCode != 0 {
			return config.NodeDetail{}, fmt.Errorf("read DRBD disk state on %s (exit %d): %v %s", node.Name, diskCode, diskErr, strings.TrimSpace(diskErrOut))
		}
		disk := strings.TrimSpace(strings.SplitN(diskOut, "/", 2)[0])
		if disk == "" {
			// Fail closed: promoting an unreadable disk could publish a stale
			// copy as the authoritative one.
			return config.NodeDetail{}, fmt.Errorf("DRBD disk state on %s is empty; refusing to promote without a known local disk state", node.Name)
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
		if primaries[0].disk != "UpToDate" {
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
		// --force is only safe for a disk that has never held authoritative
		// data (a brand-new pair is Inconsistent/Consistent). Forcing an
		// Outdated, Failed or DUnknown disk would publish stale or unreadable
		// data as the new authority.
		switch chosen.disk {
		case "Inconsistent", "Consistent":
			logging.Warn("No DRBD peer reports an UpToDate disk; forcing the initial promotion on %s (local disk %s).", chosen.node.Name, chosen.disk)
			promoteCmd += " --force"
		default:
			return config.NodeDetail{}, fmt.Errorf("refusing to promote %s: local disk state is %s, not UpToDate; resolve the disk before deploying", chosen.node.Name, chosen.disk)
		}
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
		//
		// Parse the local role and disk from their own lines: a substring
		// match on "role:Primary"/"disk:UpToDate" would also match the peer's
		// "peer role:Primary" or "peer-disk:UpToDate" line and accept a
		// Primary whose local disk is stale.
		role := drbdLocalRole(out)
		localDisk := drbdLocalDisk(out)
		connected := strings.Contains(out, "Established") || strings.Contains(out, "Connected")
		diskReady := strings.Contains(out, "peer-disk:UpToDate") || strings.Contains(out, "ds:UpToDate") || strings.Contains(out, "SyncTarget")
		if role != "Primary" || localDisk != "UpToDate" || !connected || !diskReady {
			return fmt.Errorf("replication not ready (local role %q, local disk %q): %s", role, localDisk, strings.TrimSpace(out))
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
		if _, stderr, code, err := p.pool.RunCommand(ctx, ip, fstabCmd); err != nil || code != 0 {
			logging.Warn("failed to update fstab on %s (exit %d): %v %s", ip, code, err, strings.TrimSpace(stderr))
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
		if _, stderr, code, err := p.pool.RunCommand(ctx, ip, "systemctl daemon-reload && systemctl enable --now drbd-failover-watchdog.service && systemctl enable drbd.service"); err != nil || code != 0 {
			return fmt.Errorf("failed to enable DRBD services on %s (exit %d): %v %s", ip, code, err, strings.TrimSpace(stderr))
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
		if _, stderr, code, err := p.pool.RunCommand(ctx, m.node.IP, cmd); err != nil || code != 0 {
			return fmt.Errorf("failed to enable drbd-cms-boot.service on %s (exit %d): %v %s", m.node.IP, code, err, strings.TrimSpace(stderr))
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
// authorises only the restricted local helper and pins the local sshd host keys.
// No sudoers or general operator SSH permissions are added.
func (p *Phase) setupFencing(ctx context.Context, pair []config.NodeDetail) error {
	fenceRequired := "0"
	if p.cfg.DRBD.Fencing {
		fenceRequired = "1"
	}
	logging.Info("Configuring DRBD STONITH fencing (required=%s)...", fenceRequired)

	if (p.cfg.DRBD.FenceCommand == "") != (p.cfg.DRBD.FenceStateCommand == "") {
		return fmt.Errorf("both fencing commands must be configured together")
	}
	for _, node := range pair {
		if !fenceDomainPattern.MatchString(node.Name) {
			return fmt.Errorf("invalid fencing domain %q", node.Name)
		}
	}
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
	var fenceKey, knownHosts []byte
	if p.cfg.DRBD.Fencing && !custom {
		fenceKey, knownHosts, err = p.ensureFenceKey(ctx)
		if err != nil {
			return fmt.Errorf("could not provision the hypervisor fence SSH key, so automatic promotion would never fence: %w "+
				"(set drbd.fencing: false to accept that, or configure a custom drbd.fence_command)", err)
		}
	}

	if err := p.deployFenceArtifacts(ctx, pair, fenceScript, confTmpl, fenceKey, knownHosts, fenceRequired, offCmd, stateCmd); err != nil {
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

func (p *Phase) deployFenceArtifacts(ctx context.Context, pair []config.NodeDetail, fenceScript []byte, confTmpl *template.Template, fenceKey, knownHosts []byte, fenceRequired, offCmd, stateCmd string) error {
	if len(fenceKey) > 0 && len(knownHosts) == 0 {
		return fmt.Errorf("refusing to deploy fencing key without pinned host keys")
	}
	for i, node := range pair {
		peer := pair[(i+1)%len(pair)] // DRBD cms_data is a 2-node resource

		if _, stderr, code, err := p.pool.RunCommand(ctx, node.IP, "mkdir -p /etc/drbd-fence && chmod 700 /etc/drbd-fence"); err != nil || code != 0 {
			return fmt.Errorf("failed to prepare /etc/drbd-fence on %s (exit %d): %v %s", node.Name, code, err, strings.TrimSpace(stderr))
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

			if err := p.pool.CopyContent(ctx, node.IP, knownHosts, "/etc/drbd-fence/known_hosts", 0600); err != nil {
				return fmt.Errorf("failed to copy pinned hypervisor host keys to %s: %w", node.Name, err)
			}
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

var fenceDomainPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Use the OS account, not HOME/USER from a potentially sudo-inherited environment:
// sshd looks up this account's actual home for authorized_keys.
func (p *Phase) ensureFenceKey(ctx context.Context) ([]byte, []byte, error) {
	account, err := user.Current()
	if err != nil {
		return nil, nil, err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`).MatchString(account.Username) || p.cfg.HypervisorIP() == "" {
		return nil, nil, fmt.Errorf("invalid local fencing account or hypervisor address")
	}
	return p.provisionFenceKey(ctx, account.HomeDir, "/etc/ssh")
}

// Parameters make all provisioning regressions testable without touching ~/.ssh.
func (p *Phase) provisionFenceKey(ctx context.Context, home, hostKeyDir string) ([]byte, []byte, error) {
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\r\n\x00") {
		return nil, nil, fmt.Errorf("unsafe fencing home path")
	}
	// The OS account's home is the trust boundary. Refuse unsafe ownership or
	// permissions rather than chmod the operator's general home.
	if err := fenceSafePath(home, true, true); err != nil {
		return nil, nil, err
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil || resolved != filepath.Clean(home) {
		return nil, nil, fmt.Errorf("fencing home must not contain symlinks: %s", home)
	}
	sshDir := filepath.Join(home, ".ssh")
	privateDir := filepath.Join(sshDir, "cms-ha-fence")
	for _, dir := range []string{sshDir, privateDir} {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return nil, nil, err
		}
		if err := fenceSafePath(dir, true, true); err != nil {
			return nil, nil, err
		}
	}
	if len(p.cfg.Nodes.Masters) < 2 {
		return nil, nil, fmt.Errorf("fencing requires configured masters")
	}
	var domains []string
	// Only the DRBD pair may be fenced, never the third etcd quorum member.
	for _, master := range p.cfg.Nodes.Masters[:2] {
		if !fenceDomainPattern.MatchString(master.Name) {
			return nil, nil, fmt.Errorf("invalid fencing domain %q", master.Name)
		}
		domains = append(domains, master.Name)
	}
	helperTemplate, err := cms.TemplatesFS.ReadFile("templates/drbd/fence-virsh.sh")
	if err != nil {
		return nil, nil, err
	}
	tmpl, err := template.New("fence-virsh").Parse(string(helperTemplate))
	if err != nil {
		return nil, nil, err
	}
	var helper bytes.Buffer
	if err := tmpl.Execute(&helper, domains); err != nil {
		return nil, nil, err
	}
	helperPath := filepath.Join(privateDir, "command")
	if err := fenceWriteFile(helperPath, helper.Bytes(), 0700); err != nil {
		return nil, nil, err
	}

	// Read trusted local public keys; never bootstrap trust with ssh-keyscan.
	knownHosts, err := fenceHostKeys(p.cfg.HypervisorIP(), hostKeyDir)
	if err != nil {
		return nil, nil, err
	}
	pinPath := filepath.Join(privateDir, "known_hosts")
	if err := fenceSafePath(pinPath, false, true); err != nil {
		return nil, nil, err
	}
	if pinned, err := os.ReadFile(pinPath); err == nil {
		if !bytes.Equal(pinned, knownHosts) {
			return nil, nil, fmt.Errorf("hypervisor host keys differ from persistent fencing pin %s; review host-key rotation explicitly", pinPath)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	} else if err := fenceWriteFile(pinPath, knownHosts, 0600); err != nil {
		return nil, nil, err
	}

	keyPath := filepath.Join(sshDir, "id_ed25519_fence")
	if err := fenceSafePath(keyPath, false, true); err != nil {
		return nil, nil, err
	}
	if _, err := os.Lstat(keyPath); os.IsNotExist(err) {
		if err := fenceSafePath(keyPath+".pub", false, false); err != nil {
			return nil, nil, err
		}
		out, err := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-C", "cms-ha-fence", "-f", keyPath).CombinedOutput() // #nosec G204
		if err != nil {
			return nil, nil, fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else if err != nil {
		return nil, nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	signer, err := cryptossh.ParsePrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid fencing private key: %w", err)
	}
	// Derive identity from the private key, not a stale or substituted .pub file.
	authPath := filepath.Join(sshDir, "authorized_keys")
	if err := fenceSafePath(authPath, false, true); err != nil {
		return nil, nil, err
	}
	existing, err := os.ReadFile(authPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	updated := fenceAuthorizedKeys(existing, signer.PublicKey(), helperPath)
	if err := fenceWriteFile(authPath, updated, 0600); err != nil {
		return nil, nil, err
	}
	return key, knownHosts, nil
}

// Validate each managed path before reading/replacing it. Host public keys may
// be root-owned; the local account must own its home and private artifacts.
func fenceSafePath(path string, directory, private bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && !directory {
		return nil
	}
	if err != nil {
		return err
	}
	stat := &unix.Stat_t{}
	if err := unix.Lstat(path, stat); err != nil {
		return err
	}
	uid := uint32(os.Getuid())
	unsafeMode := info.Mode().Perm()&0022 != 0

	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) || unsafeMode || (private && stat.Uid != uid) || (!private && stat.Uid != uid && stat.Uid != 0) {
		return fmt.Errorf("unsafe fencing path (ownership, type or permissions): %s", path)
	}
	if private && !directory && info.Mode().Perm()&0077 != 0 {
		// Existing authorized_keys may be public-readable; atomic replacement
		// tightens it without altering any other key entries.
		if filepath.Base(path) != "authorized_keys" {
			return fmt.Errorf("fencing file must be private: %s", path)
		}
	}
	return nil
}

func fenceWriteFile(path string, content []byte, mode os.FileMode) error {
	if err := fenceSafePath(path, false, true); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".fence-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func fenceHostKeys(host, dir string) ([]byte, error) {
	if host == "" {
		return nil, fmt.Errorf("missing hypervisor address")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "ssh_host_*_key.pub"))
	if err != nil {
		return nil, err
	}
	var pins bytes.Buffer
	for _, path := range paths {
		if err := fenceSafePath(path, false, false); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		key, _, _, rest, err := cryptossh.ParseAuthorizedKey(data)
		if err != nil || len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("invalid local sshd public host key: %s", path)
		}
		fmt.Fprintf(&pins, "%s %s", host, cryptossh.MarshalAuthorizedKey(key))
	}
	if pins.Len() == 0 {
		return nil, fmt.Errorf("no local sshd public host keys in %s", dir)
	}
	return pins.Bytes(), nil
}

func fenceAuthorizedKeys(existing []byte, key cryptossh.PublicKey, helper string) []byte {
	var result bytes.Buffer
	for _, line := range bytes.SplitAfter(existing, []byte("\n")) {
		parsed, _, _, _, err := cryptossh.ParseAuthorizedKey(line)
		if err == nil && bytes.Equal(parsed.Marshal(), key.Marshal()) {
			continue // Remove all bare, optioned and duplicate entries of this identity.
		}
		result.Write(line)
	}
	if result.Len() > 0 && result.Bytes()[result.Len()-1] != '\n' {
		result.WriteByte('\n')
	}
	command := "exec '" + strings.ReplaceAll(helper, "'", "'\\''") + "'"
	command = strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(command)
	fmt.Fprintf(&result, "restrict,no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-pty,no-user-rc,command=\"%s\" %s", command, cryptossh.MarshalAuthorizedKey(key))
	return result.Bytes()
}
