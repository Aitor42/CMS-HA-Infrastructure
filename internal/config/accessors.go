package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// JumpstartImageMode returns the configured jumpstart install path, defaulting
// to "iso" so existing configurations keep the deliverable behaviour.
func (c *Config) JumpstartImageMode() string {
	if mode := strings.ToLower(strings.TrimSpace(c.VM.JumpstartImage)); mode != "" {
		return mode
	}
	return "iso"
}

// ClientProvisioningMode returns how client nodes are installed, defaulting to
// the PXE path so the deliverable remains the default.
func (c *Config) ClientProvisioningMode() string {
	if mode := strings.ToLower(strings.TrimSpace(c.VM.Provisioning)); mode != "" {
		return mode
	}
	return "pxe"
}

// SSHUser returns the account used for every SSH connection. It defaults to
// "root" so configurations predating the non-root deploy account keep working;
// config.yaml sets "ubuntu" (the account created by the installers).
func (c *Config) SSHUser() string {
	if u := strings.TrimSpace(c.SSH.User); u != "" {
		return u
	}
	return "root"
}

// SSHKnownHostsFile returns the TOFU known_hosts path, defaulting to
// ~/.ssh/cms_known_hosts.
func (c *Config) SSHKnownHostsFile() string {
	if p := strings.TrimSpace(c.SSH.KnownHostsFile); p != "" {
		return ExpandPath(p)
	}
	return ExpandPath("~/.ssh/cms_known_hosts")
}

// RuntimeRAMMB returns the RAM a guest runs with once its OS is installed.
//
// The PXE path boots a guest with RAMInstallMB because Subiquity needs the
// headroom, then shrinks it to RAMFinalMB. A cloud-booted guest has no
// installer, so it has to start at the runtime size directly: booting the
// whole topology at the install size would overcommit the host and nothing
// would shrink it afterwards. Partial configs and tests may leave
// RAMFinalMB unset, so fall back to the install size.
func (n NodeDetail) RuntimeRAMMB() int {
	if n.RAMFinalMB > 0 {
		return n.RAMFinalMB
	}
	return n.RAMInstallMB
}

// PKICommonName returns the CA common name used to assert that a certificate
// was issued by the internal CA.
func (c *Config) PKICommonName() string {
	if c.PKI.CAName != "" {
		return c.PKI.CAName
	}
	return "CMS Local CA"
}

// LBTLSDNSNames returns the configured LB certificate DNS SANs, falling back to
// a built-in default when none are set.
func (c *Config) LBTLSDNSNames() []string {
	if len(c.PKI.CertDNSNames) > 0 {
		return c.PKI.CertDNSNames
	}
	return []string{"cms.fake-enterprise.com"}
}

// LBTLSSANs returns every SAN for the load-balancer certificate: the DNS
// names, the keepalived VIP and every balancer address. The VIP is what
// clients and the router DNAT actually use, so it must be a SAN.
func (c *Config) LBTLSSANs() []string {
	sans := append([]string{}, c.LBTLSDNSNames()...)
	if vip := c.LBVIP(); vip != "" {
		sans = append(sans, vip)
	}
	for _, lb := range c.Nodes.LBs {
		if lb.IP != "" {
			sans = append(sans, lb.IP)
		}
	}
	return sans
}

// LBTLSCertMinValidDays returns the minimum remaining validity (in days) the
// verify phase requires on the LB certificate.
func (c *Config) LBTLSCertMinValidDays() int {
	if c.PKI.CertMinValidDays > 0 {
		return c.PKI.CertMinValidDays
	}
	return 30
}

// LBTLSCertLifetimeHours returns the requested LB certificate lifetime.
func (c *Config) LBTLSCertLifetimeHours() int {
	if c.PKI.CertLifetimeHours > 0 {
		return c.PKI.CertLifetimeHours
	}
	return 2160 // 90 days
}

// HypervisorIP returns the libvirt host address on the internal network
// (network base + .254), which is where initvms configures the host bridge.
func (c *Config) HypervisorIP() string {
	_, ipnet, err := net.ParseCIDR(c.Network.Internal.CIDR)
	if err != nil {
		return ""
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return ""
	}
	ip[3] = 254
	return ip.String()
}

// FenceCommands returns the (power-off, state-query) fencing commands. When the
// config leaves them empty it derives an SSH+virsh command against the
// hypervisor as the current $USER; the literal "{domain}" placeholder is kept
// for the caller to substitute.
func (c *Config) FenceCommands() (off, state string) {
	if c.DRBD.FenceCommand != "" && c.DRBD.FenceStateCommand != "" {
		return c.DRBD.FenceCommand, c.DRBD.FenceStateCommand
	}

	host := c.HypervisorIP()
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}
	if user == "" {
		user = "root"
	}
	base := fmt.Sprintf(
		"ssh -i /etc/drbd-fence/fence_key -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 %s@%s",
		user, host,
	)
	return base + " sudo virsh destroy {domain}", base + " sudo virsh domstate {domain}"
}

// K6Image returns the pinned k6 container image.
func (c *Config) K6Image() string {
	version := c.Traffic.K6Version
	if version == "" {
		version = "0.55.0"
	}
	return "grafana/k6:" + version
}

// TrafficVHost returns the Host header used by the load profile.
func (c *Config) TrafficVHost() string {
	if v := strings.TrimSpace(c.Traffic.VHost); v != "" {
		return v
	}
	return "cms.fake-enterprise.com"
}

// BackupEnabled reports whether the backup feature is configured.
func (c *Config) BackupEnabled() bool { return c.Backup.Engine != "" }

// BackupEngine returns the configured object-store engine (default "garage").
func (c *Config) BackupEngine() string {
	if c.Backup.Engine != "" {
		return c.Backup.Engine
	}
	return "garage"
}

// GarageImage returns the pinned Garage container image.
func (c *Config) GarageImage() string {
	version := c.Backup.GarageVersion
	if version == "" {
		version = "v2.4.1"
	}
	return "dxflrs/garage:" + version
}

// BackupDataDir returns the hypervisor directory holding Garage data.
func (c *Config) BackupDataDir() string {
	if c.Backup.DataDir != "" {
		return ExpandPath(c.Backup.DataDir)
	}
	return "/var/lib/cms-garage"
}

// BackupS3Endpoint returns the S3 endpoint reachable from the cluster nodes
// (the hypervisor's internal address).
func (c *Config) BackupS3Endpoint() string {
	return fmt.Sprintf("http://%s:%d", c.HypervisorIP(), c.BackupS3Port())
}

// BackupS3Port returns the S3 API port (default 3900).
func (c *Config) BackupS3Port() int {
	if c.Backup.S3Port > 0 {
		return c.Backup.S3Port
	}
	return 3900
}

// BackupS3Region returns the S3 region advertised to clients (default "garage").
func (c *Config) BackupS3Region() string {
	if c.Backup.S3Region != "" {
		return c.Backup.S3Region
	}
	return "garage"
}

// BackupBucket returns the bucket that receives the dumps.
func (c *Config) BackupBucket() string {
	if c.Backup.Bucket != "" {
		return c.Backup.Bucket
	}
	return "cms-backups"
}

// BackupAccessKey returns the S3 access key id.
func (c *Config) BackupAccessKey() string {
	if c.Backup.AccessKey != "" {
		return c.Backup.AccessKey
	}
	return "GKcmshabackup0000000000"
}

// BackupRetentionDays returns the object retention window (default 7 days).
func (c *Config) BackupRetentionDays() int {
	if c.Backup.RetentionDays > 0 {
		return c.Backup.RetentionDays
	}
	return 7
}

// BackupSchedule returns the dump+upload CronJob schedule.
func (c *Config) BackupSchedule() string {
	if c.Backup.Schedule != "" {
		return c.Backup.Schedule
	}
	return "0 2 * * *"
}

// BackupRestoreTestSchedule returns the restore-drill CronJob schedule.
func (c *Config) BackupRestoreTestSchedule() string {
	if c.Backup.RestoreTestSchedule != "" {
		return c.Backup.RestoreTestSchedule
	}
	return "30 3 * * *"
}
