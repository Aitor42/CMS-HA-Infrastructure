package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Validate performs structural and semantic checks on the configuration.
//
// The checks are grouped into contiguous helpers so each one stays readable;
// Validate calls them in the original order because error precedence is part
// of the command's contract: the first broken field is the one reported.
func (c *Config) Validate() error {
	if err := c.validateDeploy(); err != nil {
		return err
	}
	if err := c.validateNetworks(); err != nil {
		return err
	}
	if err := c.validateTopology(); err != nil {
		return err
	}
	if err := c.validateServices(); err != nil {
		return err
	}
	if err := c.validateNodes(); err != nil {
		return err
	}
	return c.validateInterpolatedValues()
}

// validateDeploy checks the host paths and the knobs that steer the
// orchestrator before it touches any guest.
func (c *Config) validateDeploy() error {
	if c.VM.StorageDir == "" {
		return fmt.Errorf("vm.storage_dir is required")
	}
	switch c.JumpstartImageMode() {
	case "iso", "cloud":
	default:
		return fmt.Errorf("vm.jumpstart_image must be \"iso\" or \"cloud\" (got %q)", c.VM.JumpstartImage)
	}
	switch c.ClientProvisioningMode() {
	case "pxe", "cloud":
	default:
		return fmt.Errorf("vm.provisioning must be \"pxe\" or \"cloud\" (got %q)", c.VM.Provisioning)
	}
	if c.SSH.User != "" && !validUnixUser.MatchString(c.SSH.User) {
		return fmt.Errorf("ssh.user is not a valid Unix user name: %q", c.SSH.User)
	}
	if c.Deploy.InstallBatchSize < 0 {
		return fmt.Errorf("deploy.install_batch_size must not be negative (got %d)", c.Deploy.InstallBatchSize)
	}
	if c.Deploy.PuppetConcurrency < 0 {
		return fmt.Errorf("deploy.puppet_concurrency must not be negative (got %d)", c.Deploy.PuppetConcurrency)
	}
	return nil
}

// validateNetworks checks the CIDRs both subnets are built from.
func (c *Config) validateNetworks() error {
	if _, _, err := net.ParseCIDR(c.Network.Internal.CIDR); err != nil {
		return fmt.Errorf("network.internal.cidr is not a valid CIDR: %w", err)
	}
	if _, _, err := net.ParseCIDR(c.Network.Main.CIDR); err != nil {
		return fmt.Errorf("network.main.cidr is not a valid CIDR: %w", err)
	}
	return nil
}

// validateTopology checks the node counts and the floating address that make
// the deployment HA.
func (c *Config) validateTopology() error {
	if c.Nodes.Router.IP == "" {
		return fmt.Errorf("nodes.router.ip is required")
	}
	if c.Nodes.Jumpstart.IP == "" {
		return fmt.Errorf("nodes.jumpstart.ip is required")
	}
	// HA requires an etcd quorum. With an even number of members (notably the
	// old 2-node layout) quorum == member count, so losing a single master
	// makes the API server read-only and blocks DRBD/MariaDB failover. Three
	// is the minimum that tolerates one failure; keep it odd so extra members
	// increase fault tolerance rather than just raising the quorum threshold.
	if len(c.Nodes.Masters) < 3 {
		return fmt.Errorf("at least 3 master nodes are required for etcd quorum (got %d)", len(c.Nodes.Masters))
	}
	if len(c.Nodes.Masters)%2 == 0 {
		return fmt.Errorf("an odd number of master nodes is required for etcd quorum (got %d)", len(c.Nodes.Masters))
	}
	// A half-configured fence would silently disable promotion (or worse,
	// promote without fencing), so require both commands together.
	if (c.DRBD.FenceCommand == "") != (c.DRBD.FenceStateCommand == "") {
		return fmt.Errorf("drbd.fence_command and drbd.fence_state_command must be set together (or both left empty to auto-derive)")
	}
	if len(c.Nodes.LBs) < 2 {
		return fmt.Errorf("at least 2 load balancer nodes are required for HA (got %d)", len(c.Nodes.LBs))
	}
	if err := c.validateMainVIP(); err != nil {
		return err
	}
	if len(c.Nodes.CMSFrontends) < 1 {
		return fmt.Errorf("at least 1 CMS frontend node is required")
	}
	return nil
}

func (c *Config) validateMainVIP() error {
	if c.Network.Main.VIP == "" {
		return fmt.Errorf("network.main.vip is required (keepalived floating address)")
	}
	if ip := net.ParseIP(c.Network.Main.VIP); ip == nil || ip.To4() == nil {
		return fmt.Errorf("network.main.vip is not a valid IPv4 address: %q", c.Network.Main.VIP)
	}
	for _, n := range c.AllNodes() {
		if n.IP == c.Network.Main.VIP {
			return fmt.Errorf("network.main.vip %s collides with node %s", c.Network.Main.VIP, n.Name)
		}
	}
	return nil
}

// validateServices checks the PKI, backup and traffic settings that later
// phases interpolate into remote commands.
func (c *Config) validateServices() error {
	if err := c.validatePKI(); err != nil {
		return err
	}
	if err := c.validateBackup(); err != nil {
		return err
	}
	if err := c.validateMonitoring(); err != nil {
		return err
	}
	return c.validateTraffic()
}

func (c *Config) validatePKI() error {
	// A certificate whose total lifetime is shorter than the minimum validity
	// asserted by verify would fail as soon as it is issued.
	if c.PKI.CertMinValidDays > 0 && c.PKI.CertLifetimeHours > 0 &&
		c.PKI.CertLifetimeHours <= c.PKI.CertMinValidDays*24 {
		return fmt.Errorf("pki.cert_lifetime_hours (%d) must exceed pki.cert_min_valid_days*24 (%d)",
			c.PKI.CertLifetimeHours, c.PKI.CertMinValidDays*24)
	}
	for _, dns := range c.PKI.CertDNSNames {
		if !fqdnPattern.MatchString(dns) {
			return fmt.Errorf("pki.cert_dns_names entry is not a valid DNS name: %q", dns)
		}
	}
	return nil
}

func (c *Config) validateBackup() error {
	if c.Backup.Engine != "" && c.Backup.Engine != "garage" {
		return fmt.Errorf("backup.engine %q is not supported (supported: garage)", c.Backup.Engine)
	}
	if c.Backup.S3Port < 0 || c.Backup.S3Port > 65535 {
		return fmt.Errorf("backup.s3_port out of range: %d", c.Backup.S3Port)
	}
	if c.Backup.RetentionDays < 0 {
		return fmt.Errorf("backup.retention_days must be >= 0")
	}
	for _, sched := range []string{c.Backup.Schedule, c.Backup.RestoreTestSchedule} {
		if sched != "" && !cronPattern.MatchString(sched) {
			return fmt.Errorf("backup schedule %q contains unsupported characters", sched)
		}
	}
	return nil
}

func (c *Config) validateMonitoring() error {
	// The password is written to a systemd EnvironmentFile without quoting, so
	// keep it to characters that cannot break or inject an environment line.
	if pw := c.Monitoring.GrafanaAdminPassword; pw != "" && !grafanaPasswordPattern.MatchString(pw) {
		return fmt.Errorf("monitoring.grafana_admin_password contains unsupported characters (use letters, digits, '.', '_', '~' or '-')")
	}
	return nil
}

func (c *Config) validateTraffic() error {
	if v := strings.TrimSpace(c.Traffic.K6Version); v == "latest" {
		return fmt.Errorf("traffic.k6_version must be pinned to a specific tag (got %q)", v)
	}
	if rw := strings.TrimSpace(c.Traffic.PrometheusRWURL); rw != "" {
		if u, err := url.Parse(rw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("traffic.prometheus_rw_url must be an http(s) URL: %q", rw)
		}
	}
	return nil
}

// validateNodes checks the generated hotdesk range and the shape of every
// node identifier that ends up in an SSH command or Cobbler script.
func (c *Config) validateNodes() error {
	if err := c.validateHotdesks(); err != nil {
		return err
	}
	return c.validateNodeFields()
}

func (c *Config) validateHotdesks() error {
	// Hotdesk IPs are generated as <network>.<octet>: detect overflow before
	// the node sweep in validateNodeFields so this clearer message wins
	// (generation happens inside AllNodes and would otherwise surface as
	// "invalid IPv4 .256").
	if c.Nodes.Hotdesks.Count > 0 {
		base := c.Nodes.Hotdesks.BaseIPOctet
		if base == 0 || base == 201 {
			base = 201
		}
		if base+c.Nodes.Hotdesks.Count-1 > 254 {
			return fmt.Errorf("hotdesk range overflows: base octet %d with %d nodes exceeds 254", base, c.Nodes.Hotdesks.Count)
		}
	}
	return nil
}

func (c *Config) validateNodeFields() error {
	// These fields are interpolated into remote SSH commands, Cobbler
	// registration scripts and awk filters: an unvalidated value breaks or
	// manipulates far-away infrastructure, so their shape must be exact.
	for _, n := range c.AllNodes() {
		if n.Name != "" && !nodeNamePattern.MatchString(n.Name) {
			return fmt.Errorf("node has an invalid name (letters, digits, dots, underscores, hyphens only): %q", n.Name)
		}
		if n.IP != "" {
			if ip := net.ParseIP(n.IP); ip == nil || ip.To4() == nil {
				return fmt.Errorf("node %s has an invalid IPv4 address: %q", n.Name, n.IP)
			}
		}
		if n.FQDN != "" && !fqdnPattern.MatchString(n.FQDN) {
			return fmt.Errorf("node %s has an invalid FQDN (letters, digits, dots, hyphens only): %q", n.Name, n.FQDN)
		}
		if n.MAC != "" && !macPattern.MatchString(n.MAC) {
			return fmt.Errorf("node %s has an invalid MAC address: %q", n.Name, n.MAC)
		}
		// mac_wan is interpolated into an awk filter on the router too.
		if n.MACWAN != "" && !macPattern.MatchString(n.MACWAN) {
			return fmt.Errorf("node %s has an invalid WAN MAC address: %q", n.Name, n.MACWAN)
		}
	}
	return nil
}

// validateInterpolatedValues checks the remaining values that are embedded in
// shell commands or download URLs without quoting.
func (c *Config) validateInterpolatedValues() error {
	// These are interpolated unquoted into the backup shell command: a dot
	// would be parsed as db.table and any other character would break or
	// inject into the command, so restrict them to plain SQL identifiers.
	for field, v := range map[string]string{"database.user": c.Database.User, "database.name": c.Database.Name} {
		if v != "" && !dbIdentifierPattern.MatchString(v) {
			return fmt.Errorf("%s must contain only letters, digits, or underscores: %q", field, v)
		}
	}

	// The step versions are interpolated into the download URL that the jumpstart
	// runs, so they must be plain release versions.
	for field, v := range map[string]string{"pki.step_version": c.PKI.StepVersion, "pki.step_ca_version": c.PKI.StepCAVersion} {
		if v != "" && !stepVersionPattern.MatchString(v) {
			return fmt.Errorf("%s must be a plain release version such as \"0.31.0\": %q", field, v)
		}
	}
	return nil
}

// ValidatePartial performs the subset of checks that still matters for
// commands acting on whatever resources are currently present, rather than on
// a complete deployment: teardown (cleanup/destroy), VM lifecycle (vm
// start/stop/shrink/recreate/fix-boot-order), status and repair. Those must
// work even when the topology in config.yaml is partial or temporarily broken
// (for example a node was dropped, or its IP is half-edited), so unlike
// Validate it does not require a complete deployment topology. Only the values
// that steer file deletion, libvirt lookups and node identification are still
// checked, because a malformed one there could remove or touch the wrong
// resources.
func (c *Config) ValidatePartial() error {
	if c.VM.StorageDir == "" {
		return fmt.Errorf("vm.storage_dir is required to locate disk images")
	}
	for _, n := range c.AllNodes() {
		if n.Name != "" && !nodeNamePattern.MatchString(n.Name) {
			return fmt.Errorf("node has an invalid name (letters, digits, dots, underscores, hyphens only): %q", n.Name)
		}
	}
	return nil
}

var fqdnPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
var macPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// ValidMAC reports whether s is a colon-separated MAC address in the exact
// shape the deploy interpolates into remote commands (awk filters).
func ValidMAC(s string) bool { return macPattern.MatchString(s) }

// stepVersionPattern matches a plain release version ("0.31.0", optionally
// prefixed with "v"), the only shape safe to interpolate into the release URL.
var stepVersionPattern = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){1,3}$`)

var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var dbIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
var cronPattern = regexp.MustCompile(`^[0-9A-Za-z*,/ @-]+$`)
var grafanaPasswordPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// validUnixUser matches a conservative portable POSIX user name (lowercase
// letters, digits, underscore and hyphen, not starting with a digit or hyphen).
var validUnixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
