package cobbler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
	"github.com/pkg/sftp"
)

type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string { return "01-setup-cobbler" }
func (p *Phase) Description() string {
	return "Install and configure Cobbler PXE provisioning server on Jumpstart"
}

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	jumpIP := p.cfg.Nodes.Jumpstart.IP

	if err := p.waitForJumpstartSSH(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed waiting for Jumpstart SSH: %w", err)
	}

	logging.Info("Transferring Ubuntu ISO...")
	if err := p.transferISO(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed to transfer ISO: %w", err)
	}

	logging.Info("Uploading template files...")
	if err := p.uploadTemplates(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed to upload templates: %w", err)
	}

	logging.Info("Installing and configuring Cobbler...")
	if err := p.installCobbler(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed to install Cobbler: %w", err)
	}

	logging.Info("Verifying Cobbler services...")
	if err := p.verifyCobbler(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed to verify Cobbler: %w", err)
	}

	logging.Success("Cobbler setup completed successfully")
	return nil
}

// waitForJumpstartSSH waits until the Jumpstart node accepts SSH as the
// configured non-root deploy account (`ssh.user`). Direct root logins are
// disabled by the installers, so the orchestrator never tries to enable them;
// privileged commands are elevated transparently with sudo (see docs/SECURITY.md).
func (p *Phase) waitForJumpstartSSH(ctx context.Context, jumpIP string) error {
	logging.Info("Waiting for SSH on Jumpstart (%s) as %q...", jumpIP, p.cfg.SSHUser())
	if err := p.pool.WaitForSSH(ctx, jumpIP, 5*time.Minute); err != nil {
		return fmt.Errorf("timed out waiting for SSH on %s: %w", jumpIP, err)
	}
	logging.Success("SSH accessible on %s", jumpIP)
	return nil
}

func (p *Phase) transferISO(ctx context.Context, jumpIP string) error {
	pattern := filepath.Join(p.cfg.VM.StorageDir, "ubuntu-24.04*.iso")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("could not find Ubuntu ISO in %s", p.cfg.VM.StorageDir)
	}

	isoPath := ""
	for _, match := range matches {
		info, err := os.Stat(match)
		if err == nil && info.Size() > 1024*1024*1024 { // > 1GB
			isoPath = match
			break
		}
	}
	if isoPath == "" {
		return fmt.Errorf("could not find an Ubuntu ISO > 1GB in %s", p.cfg.VM.StorageDir)
	}

	remotePath := "/var/lib/cobbler/isos/ubuntu-24.04-live-server-amd64.iso"

	// Check if already present on Jumpstart
	_, _, code, _ := p.pool.RunCommand(ctx, jumpIP, fmt.Sprintf("[ -f %s ] && [ $(stat -c%%s %s 2>/dev/null || echo 0) -gt 1000000000 ]", remotePath, remotePath))
	if code == 0 {
		logging.Success("Ubuntu ISO already exists on Jumpstart (%s), skipping transfer", remotePath)
		return nil
	}

	_, _, _, err = p.pool.RunCommand(ctx, jumpIP, "mkdir -p /var/lib/cobbler/isos")
	if err != nil {
		return err
	}

	logging.Info("Uploading %s to %s", isoPath, remotePath)
	return p.pool.CopyFile(ctx, jumpIP, isoPath, remotePath)
}

func (p *Phase) uploadTemplates(ctx context.Context, jumpIP string) error {
	templates := map[string]string{
		"templates/systemd/no-rate-limit.conf":            "/tmp/tpl_no_rate_limit.conf",
		"templates/systemd/isc-dhcp-timeout.conf":         "/tmp/tpl_isc_dhcp_timeout.conf",
		"templates/systemd/cobblerd-timeout.conf":         "/tmp/tpl_cobblerd_timeout.conf",
		"templates/cobbler/settings-patch.py":             "/tmp/tpl_settings_patch.py",
		"templates/cobbler/dhcp.template":                 "/tmp/tpl_dhcp.template",
		"templates/cobbler/named.template":                "/tmp/tpl_named.template",
		"templates/cobbler/ubuntu-24.04-autoinstall.yaml": "/tmp/tpl_autoinstall.yaml",
		"templates/cobbler/casper_initrd_tcp.sh":          "/tmp/casper_initrd_tcp.sh",
	}

	return p.pool.WithSFTP(ctx, jumpIP, func(sftpClient *sftp.Client) error {
		for src, dst := range templates {
			content, err := cms.TemplatesFS.ReadFile(src)
			if err != nil {
				return fmt.Errorf("failed to read template %s: %w", src, err)
			}
			f, err := sftpClient.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
			if err != nil {
				return fmt.Errorf("failed to open remote file %s: %w", dst, err)
			}
			_ = f.Chmod(0644)
			_, copyErr := f.Write(content)
			_ = f.Close()
			if copyErr != nil {
				return fmt.Errorf("failed to write template %s: %w", dst, copyErr)
			}
		}
		return nil
	})
}

func (p *Phase) installCobbler(ctx context.Context, jumpIP string) error {
	logging.Info("Waiting for package manager lock to be released...")
	if err := p.pool.WaitForAptLock(ctx, jumpIP); err != nil {
		logging.Warn("Apt lock wait returned: %v (continuing)", err)
	}

	steps := []string{
		`add-apt-repository -y universe 2>/dev/null || true && apt-get update -qq`,

		`COBBLER_REPO_24="https://download.opensuse.org/repositories/systemsmanagement:/cobbler:/release33/xUbuntu_24.04"
COBBLER_REPO_22="https://download.opensuse.org/repositories/systemsmanagement:/cobbler:/release33/xUbuntu_22.04"
if ! grep -q "systemsmanagement.*cobbler" /etc/apt/sources.list /etc/apt/sources.list.d/* 2>/dev/null; then
    curl -fsSL "${COBBLER_REPO_24}/Release.key" 2>/dev/null | gpg --dearmor --yes -o /etc/apt/trusted.gpg.d/cobbler.gpg 2>/dev/null || true
    if curl -sf --max-time 15 "${COBBLER_REPO_24}/Release" >/dev/null 2>&1; then
        echo "deb [signed-by=/etc/apt/trusted.gpg.d/cobbler.gpg] ${COBBLER_REPO_24}/ /" > /etc/apt/sources.list.d/cobbler.list
        apt-get update -qq 2>/dev/null || true
    fi
    if ! apt-cache show cobbler >/dev/null 2>&1; then
        echo "deb [trusted=yes] ${COBBLER_REPO_22}/ /" > /etc/apt/sources.list.d/cobbler.list
        apt-get update -qq 2>/dev/null || true
    fi
fi`,

		`DEBIAN_FRONTEND=noninteractive apt-get install -y cobbler apache2 isc-dhcp-server tftpd-hpa nfs-kernel-server libapache2-mod-wsgi-py3 python3-yaml bind9 bind9utils pxelinux ipxe shim-signed grub-efi-amd64-signed syslinux-common curl wget rsync || {
    DEBIAN_FRONTEND=noninteractive apt-get install -y --fix-broken cobbler apache2 isc-dhcp-server tftpd-hpa nfs-kernel-server libapache2-mod-wsgi-py3 python3-yaml bind9 bind9utils pxelinux ipxe syslinux-common curl wget rsync
}`,

		`if ! python3 -c "import django" &>/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y python3-pip 2>/dev/null || true
    pip3 install "django>=3.2,<5" --break-system-packages 2>/dev/null || pip3 install "django>=3.2,<5" 2>/dev/null || true
fi`,

		`for svc in named.service isc-dhcp-server.service cobblerd.service; do
    svc_dir="/etc/systemd/system/${svc}.d"
    mkdir -p "$svc_dir"
    cp /tmp/tpl_no_rate_limit.conf "${svc_dir}/override.conf"
done
mkdir -p /etc/systemd/system/isc-dhcp-server.service.d
mkdir -p /etc/systemd/system/cobblerd.service.d
cat /tmp/tpl_isc_dhcp_timeout.conf >> /etc/systemd/system/isc-dhcp-server.service.d/override.conf
cat /tmp/tpl_cobblerd_timeout.conf >> /etc/systemd/system/cobblerd.service.d/override.conf
systemctl daemon-reload`,

		`INT_IF=$(ip -o link show 2>/dev/null | awk -F': ' '/52:54:00:10:00:01/{print $2}' | head -1)
[ -z "$INT_IF" ] && INT_IF="enp1s0"
MAIN_IF=$(ip -o link show 2>/dev/null | awk -F': ' '/52:54:00:10:02:0a/{print $2}' | head -1)
[ -z "$MAIN_IF" ] && MAIN_IF="enp2s0"
IFACES_LINE="INTERFACESv4=\"${INT_IF} ${MAIN_IF}\""
if grep -q 'INTERFACESv4=' /etc/default/isc-dhcp-server 2>/dev/null; then
    sed -i "s|INTERFACESv4=.*|${IFACES_LINE}|" /etc/default/isc-dhcp-server
else
    echo "${IFACES_LINE}" >> /etc/default/isc-dhcp-server
fi`,

		`python3 /tmp/tpl_settings_patch.py`,

		`cp /tmp/tpl_dhcp.template /etc/cobbler/dhcp.template
cp /tmp/tpl_named.template /etc/cobbler/named.template
mkdir -p /var/cache/bind/data
chown -R bind:bind /var/cache/bind`,

		`if [ -f /etc/default/tftpd-hpa ]; then
    sed -i 's|^TFTP_DIRECTORY=.*|TFTP_DIRECTORY="/srv/tftpboot"|' /etc/default/tftpd-hpa
    sed -i 's|^TFTP_OPTIONS=.*|TFTP_OPTIONS="--secure --create"|' /etc/default/tftpd-hpa
fi
mkdir -p /srv/tftpboot`,

		`mkdir -p /var/www/cobbler/distro_mirror/ubuntu-24.04
NFS_EXPORT="/var/www/cobbler/distro_mirror/ubuntu-24.04 *(ro,async,no_root_squash,no_subtree_check)"
if ! grep -qF "/var/www/cobbler/distro_mirror/ubuntu-24.04" /etc/exports 2>/dev/null; then
    echo "${NFS_EXPORT}" >> /etc/exports
fi`,

		`a2enmod wsgi 2>/dev/null || true
a2enconf cobbler 2>/dev/null || true`,

		`for svc in tftpd-hpa apache2 cobblerd nfs-kernel-server named; do
    systemctl restart $svc && systemctl enable $svc
done`,
	}

	for i, step := range steps {
		logging.Info("Executing install step %d/%d...", i+1, len(steps))
		_, err := p.pool.RunScript(ctx, jumpIP, step)
		if err != nil {
			return fmt.Errorf("install step %d failed: %w", i+1, err)
		}
	}

	logging.Info("Waiting for cobblerd to become active...")
	err := retry.PollContext(ctx, retry.Config{MaxAttempts: 40, Interval: 3 * time.Second, MaxInterval: 15 * time.Second, Timeout: 2 * time.Minute}, func(attemptCtx context.Context) (bool, error) {
		_, _, code, _ := p.pool.RunCommand(attemptCtx, jumpIP, "systemctl is-active --quiet cobblerd")
		return code == 0, nil
	})
	if err != nil {
		return fmt.Errorf("cobblerd failed to start: %w", err)
	}

	postSteps := []string{
		`set -e
mkdir -p /var/www/cobbler/pub
[ -f /root/.ssh/id_ed25519 ] || ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519
{ cat /root/.ssh/id_ed25519.pub; cat /root/.ssh/authorized_keys 2>/dev/null || true; } | awk '!seen[$0]++' > /var/www/cobbler/pub/authorized_keys
chmod 644 /var/www/cobbler/pub/authorized_keys`,

		`set -e
ISO_FILE="/var/lib/cobbler/isos/ubuntu-24.04-live-server-amd64.iso"
MOUNT_POINT="/mnt/ubuntu-24.04"
cobbler system list 2>/dev/null | awk '{print $1}' | while read -r sys; do
    [ -n "$sys" ] && cobbler system remove --name="$sys" &>/dev/null || true
done
cobbler profile remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true
cobbler distro  remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true

mkdir -p "$MOUNT_POINT"
if [ -f "$ISO_FILE" ]; then
    # mount/rsync failures must abort: an incomplete mirror breaks
    # 'cobbler distro add' or produces a PXE installer that never boots.
    mount -o loop "$ISO_FILE" "$MOUNT_POINT"
    mkdir -p /var/www/cobbler/distro_mirror/ubuntu-24.04
    rsync -a "${MOUNT_POINT}/" /var/www/cobbler/distro_mirror/ubuntu-24.04/
    umount "$MOUNT_POINT"
fi

# Casper (BusyBox nfsmount) mounts the PXE root via NFSv3/UDP by default and
# ignores all command-line NFS options; concurrent clients then die in
# SQASHFS I/O errors and install-batches never sees SSH. The helper verifies
# the mirror (md5sum -c) and appends a cpio segment to casper/initrd that
# forces the mount over TCP (ro,tcp); a failure here must abort the phase.
install -m 0755 /tmp/casper_initrd_tcp.sh /usr/local/bin/casper_initrd_tcp.sh
/usr/local/bin/casper_initrd_tcp.sh /var/www/cobbler/distro_mirror/ubuntu-24.04

cobbler distro add --name="ubuntu-24.04-x86_64" --kernel="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/vmlinuz" --initrd="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/initrd" --breed=ubuntu`,

		`set -e
mkdir -p /etc/cobbler/autoinstall_templates /var/lib/cobbler/templates
cp /tmp/tpl_autoinstall.yaml /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml
cp /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml /var/lib/cobbler/templates/ubuntu-24.04-autoinstall.yaml

cobbler profile add --name="ubuntu-24.04-x86_64" --distro="ubuntu-24.04-x86_64" --autoinstall="ubuntu-24.04-autoinstall.yaml" --autoinstall-meta="hostname=default"`,

		`set -e
cobbler mkloaders 2>/dev/null || true
cobbler sync`,
	}

	for i, step := range postSteps {
		logging.Info("Executing post-install step %d/%d...", i+1, len(postSteps))
		_, err := p.pool.RunScript(ctx, jumpIP, step)
		if err != nil {
			return fmt.Errorf("post-install step %d failed: %w", i+1, err)
		}
	}

	return nil
}

func (p *Phase) verifyCobbler(ctx context.Context, jumpIP string) error {
	services := []string{"cobblerd", "apache2", "tftpd-hpa", "isc-dhcp-server", "named", "nfs-kernel-server"}
	// A single `systemctl is-active` call checks every unit at once (one SSH
	// round-trip per poll instead of one per service) and exits non-zero while
	// any of them is inactive.
	cmd := "systemctl is-active " + strings.Join(services, " ")
	var out string
	err := retry.PollContext(ctx, retry.Config{MaxAttempts: 10, Interval: 1 * time.Second, Timeout: 30 * time.Second}, func(attemptCtx context.Context) (bool, error) {
		o, _, code, _ := p.pool.RunCommand(attemptCtx, jumpIP, cmd)
		out = o
		return code == 0, nil
	})
	if err != nil {
		var failed []string
		states := strings.Fields(strings.TrimSpace(out))
		for i, svc := range services {
			if i < len(states) && states[i] != "active" {
				failed = append(failed, fmt.Sprintf("%s (%s)", svc, states[i]))
			}
		}
		if len(failed) == 0 {
			return fmt.Errorf("cobbler verification failed: %w", err)
		}
		return fmt.Errorf("cobbler verification failed: inactive services: %s", strings.Join(failed, ", "))
	}
	return nil
}
