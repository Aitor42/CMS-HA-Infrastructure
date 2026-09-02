package cobbler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/logging"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/phases"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/retry"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ssh"
)

type Phase struct {
	cfg  *config.Config
	pool *ssh.Pool
}

func NewPhase(cfg *config.Config, pool *ssh.Pool) phases.Phase {
	return &Phase{cfg: cfg, pool: pool}
}

func (p *Phase) Name() string        { return "01-setup-cobbler" }
func (p *Phase) Description() string { return "Install and configure Cobbler PXE provisioning server on Jumpstart" }

func (p *Phase) Run(ctx context.Context) error {
	timer := logging.PhaseStart(p.Name())
	defer timer.End()

	jumpIP := p.cfg.Nodes.Jumpstart.IP
	
	logging.Info("Elevating root SSH on Jumpstart...")
	if err := p.elevateRootSSH(ctx, jumpIP); err != nil {
		return fmt.Errorf("failed to elevate root SSH: %w", err)
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

func (p *Phase) elevateRootSSH(ctx context.Context, jumpIP string) error {
	script := `
mkdir -p /root/.ssh
for f in /home/*/.ssh/authorized_keys; do
	if [ -f "$f" ]; then
		cat "$f" >> /root/.ssh/authorized_keys
	fi
done
sort -u /root/.ssh/authorized_keys -o /root/.ssh/authorized_keys 2>/dev/null || true
chmod 700 /root/.ssh
chmod 600 /root/.ssh/authorized_keys
`
	_, err := p.pool.RunScript(ctx, jumpIP, script)
	return err
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
	
	// Create dir
	_, _, _, err = p.pool.RunCommand(ctx, jumpIP, "mkdir -p /var/lib/cobbler/isos")
	if err != nil {
		return err
	}
	
	logging.Info("Uploading %s to %s", isoPath, remotePath)
	return p.pool.CopyFile(ctx, jumpIP, isoPath, remotePath)
}

func (p *Phase) uploadTemplates(ctx context.Context, jumpIP string) error {
	templates := map[string]string{
		"templates/systemd/no-rate-limit.conf":           "/tmp/tpl_no_rate_limit.conf",
		"templates/systemd/isc-dhcp-timeout.conf":        "/tmp/tpl_isc_dhcp_timeout.conf",
		"templates/systemd/cobblerd-timeout.conf":        "/tmp/tpl_cobblerd_timeout.conf",
		"templates/cobbler/settings-patch.py":            "/tmp/tpl_settings_patch.py",
		"templates/cobbler/dhcp.template":                "/tmp/tpl_dhcp.template",
		"templates/cobbler/named.template":               "/tmp/tpl_named.template",
		"templates/cobbler/ubuntu-24.04-autoinstall.yaml": "/tmp/tpl_autoinstall.yaml",
	}

	for src, dst := range templates {
		content, err := cms.TemplatesFS.ReadFile(src)
		if err != nil {
			return fmt.Errorf("failed to read template %s: %w", src, err)
		}
		if err := p.pool.CopyContent(ctx, jumpIP, content, dst, 0644); err != nil {
			return fmt.Errorf("failed to upload %s to %s: %w", src, dst, err)
		}
	}
	return nil
}

func (p *Phase) installCobbler(ctx context.Context, jumpIP string) error {
	steps := []string{
		`add-apt-repository -y universe 2>/dev/null || true && apt-get update -qq`,
		
		`if ! grep -q "cobbler/cobbler" /etc/apt/sources.list /etc/apt/sources.list.d/* 2>/dev/null; then
    wget -qO - https://download.opensuse.org/repositories/home:/cobbler:/cobbler/xUbuntu_24.04/Release.key | gpg --dearmor -o /etc/apt/trusted.gpg.d/cobbler.gpg
    echo "deb http://download.opensuse.org/repositories/home:/cobbler:/cobbler/xUbuntu_24.04/ /" > /etc/apt/sources.list.d/cobbler.list
    apt-get update -qq || true
fi`,
		
		`DEBIAN_FRONTEND=noninteractive apt-get install -y cobbler apache2 isc-dhcp-server tftpd-hpa nfs-kernel-server libapache2-mod-wsgi-py3 python3-yaml bind9 bind9utils pxelinux ipxe shim-signed grub-efi-amd64-signed syslinux-common curl wget rsync`,
		
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

	// Wait for cobblerd
	logging.Info("Waiting for cobblerd to become active...")
	err := retry.Poll(ctx, retry.Config{MaxAttempts: 40, Interval: 3 * time.Second}, func() (bool, error) {
		_, _, code, _ := p.pool.RunCommand(ctx, jumpIP, "systemctl is-active --quiet cobblerd")
		return code == 0, nil
	})
	if err != nil {
		return fmt.Errorf("cobblerd failed to start: %w", err)
	}

	postSteps := []string{
		`mkdir -p /var/www/cobbler/pub
[ -f /root/.ssh/id_ed25519 ] || ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519
{ cat /root/.ssh/id_ed25519.pub; cat /root/.ssh/authorized_keys 2>/dev/null || true; } | awk '!seen[$0]++' > /var/www/cobbler/pub/authorized_keys
chmod 644 /var/www/cobbler/pub/authorized_keys`,

		`ISO_FILE="/var/lib/cobbler/isos/ubuntu-24.04-live-server-amd64.iso"
MOUNT_POINT="/mnt/ubuntu-24.04"
cobbler system list 2>/dev/null | awk '{print $1}' | while read -r sys; do
    [ -n "$sys" ] && cobbler system remove --name="$sys" &>/dev/null || true
done
cobbler profile remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true
cobbler distro  remove --name="ubuntu-24.04-x86_64" 2>/dev/null || true

mkdir -p "$MOUNT_POINT"
mount -o loop "$ISO_FILE" "$MOUNT_POINT" 2>/dev/null || true
mkdir -p /var/www/cobbler/distro_mirror/ubuntu-24.04
rsync -a "${MOUNT_POINT}/" /var/www/cobbler/distro_mirror/ubuntu-24.04/ 2>/dev/null || true
umount "$MOUNT_POINT" 2>/dev/null || true

cobbler distro add --name="ubuntu-24.04-x86_64" --kernel="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/vmlinuz" --initrd="/var/www/cobbler/distro_mirror/ubuntu-24.04/casper/initrd" --breed=ubuntu`,

		`mkdir -p /etc/cobbler/autoinstall_templates /var/lib/cobbler/templates
cp /tmp/tpl_autoinstall.yaml /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml
cp /etc/cobbler/autoinstall_templates/ubuntu-24.04-autoinstall.yaml /var/lib/cobbler/templates/ubuntu-24.04-autoinstall.yaml

cobbler profile add --name="ubuntu-24.04-x86_64" --distro="ubuntu-24.04-x86_64" --autoinstall="ubuntu-24.04-autoinstall.yaml" --autoinstall-meta="hostname=default"`,

		`cobbler mkloaders 2>/dev/null || true
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
	for _, svc := range services {
		_, _, code, _ := p.pool.RunCommand(ctx, jumpIP, fmt.Sprintf("systemctl is-active --quiet %s", svc))
		if code != 0 {
			logging.Warn("Service %s is not active", svc)
		}
	}
	return nil
}
