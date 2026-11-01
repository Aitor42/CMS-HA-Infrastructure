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
	if c.SSH.User == "root" {
		return fmt.Errorf("ssh.user must be a non-root deploy account; every installer disables direct root logins")
	}
	if c.Deploy.InstallBatchSize < 0 {
		return fmt.Errorf("deploy.install_batch_size must not be negative (got %d)", c.Deploy.InstallBatchSize)
	}
	if c.Deploy.PuppetConcurrency < 0 {
		return fmt.Errorf("deploy.puppet_concurrency must not be negative (got %d)", c.Deploy.PuppetConcurrency)
	}
	return nil
}

// validateNetworks checks the CIDRs both subnets are built from, and the
// interface and DNS domain names that go into the libvirt network XML.
func (c *Config) validateNetworks() error {
	for _, n := range []struct {
		name string
		net  NetworkDetail
	}{
		{"internal", c.Network.Internal},
		{"main", c.Network.Main},
	} {
		_, ipnet, err := net.ParseCIDR(n.net.CIDR)
		if err != nil {
			return fmt.Errorf("network.%s.cidr is not a valid CIDR: %w", n.name, err)
		}
		// A gateway outside its own subnet silently produces a router that
		// cannot reach the clients it is supposed to route for.
		if gw := strings.TrimSpace(n.net.Gateway); gw != "" {
			ip := net.ParseIP(gw)
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("network.%s.gateway is not a valid IPv4 address: %q", n.name, gw)
			}
			if !ipnet.Contains(ip) {
				return fmt.Errorf("network.%s.gateway %s is outside network.%s.cidr %s", n.name, gw, n.name, n.net.CIDR)
			}
		}
		if n.net.Bridge != "" && !bridgePattern.MatchString(n.net.Bridge) {
			return fmt.Errorf("network.%s.bridge is not a valid Linux bridge name (1-15 letters, digits, '.', '_' or '-', starting with a letter or digit): %q",
				n.name, n.net.Bridge)
		}
		if n.net.Domain != "" && !fqdnPattern.MatchString(n.net.Domain) {
			return fmt.Errorf("network.%s.domain is not a valid DNS domain: %q", n.name, n.net.Domain)
		}
	}
	// The router *is* the internal gateway: router-bootstrap installs
	// network.internal.gateway on it, so a different nodes.router.ip would
	// leave the orchestrator connecting to an address the router does not own.
	if gw := strings.TrimSpace(c.Network.Internal.Gateway); gw != "" && c.Nodes.Router.IP != "" && c.Nodes.Router.IP != gw {
		return fmt.Errorf("nodes.router.ip (%s) must equal network.internal.gateway (%s): the router is the internal gateway", c.Nodes.Router.IP, gw)
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
	// pki.domain becomes the CA URL host and one of the CA certificate SANs.
	if c.PKI.Domain != "" && !fqdnPattern.MatchString(c.PKI.Domain) {
		return fmt.Errorf("pki.domain is not a valid DNS name: %q", c.PKI.Domain)
	}
	// pki.ca_name is interpolated into `step ca init --name="..."` and is later
	// matched against the issuer in the renewal script, so it must not be able
	// to close the quote or inject shell syntax.
	if c.PKI.CAName != "" && !caNamePattern.MatchString(c.PKI.CAName) {
		return fmt.Errorf("pki.ca_name must be 1-64 characters of letters, digits, spaces, dots, dashes or underscores, starting and ending with a letter or digit: %q", c.PKI.CAName)
	}
	if c.PKI.CAPort != 0 && (c.PKI.CAPort < 1 || c.PKI.CAPort > 65535) {
		return fmt.Errorf("pki.ca_port must be between 1 and 65535 (got %d)", c.PKI.CAPort)
	}
	if c.PKI.CertLifetimeHours < 0 {
		return fmt.Errorf("pki.cert_lifetime_hours must not be negative (got %d)", c.PKI.CertLifetimeHours)
	}
	// step-ca rejects --not-after values above the provisioner's x509-max-dur,
	// so a larger request would fail only after the CA is up and configured.
	if c.PKI.CertLifetimeHours > MaxPKICertLifetimeHours {
		return fmt.Errorf("pki.cert_lifetime_hours must not exceed the provisioner's %dh maximum (got %d)",
			MaxPKICertLifetimeHours, c.PKI.CertLifetimeHours)
	}
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
	// These values are written to the container's --env-file, which is line
	// based: an embedded newline would inject an extra variable or truncate
	// the credential.
	for field, value := range map[string]string{
		"backup.access_key": c.Backup.AccessKey,
		"backup.secret_key": c.Backup.SecretKey,
		"backup.bucket":     c.Backup.Bucket,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s cannot contain line breaks", field)
		}
	}
	// Garage refuses to start when the default access key or secret does not
	// satisfy Key::import, and the container then crash-loops before the bucket
	// exists. Reject the same shapes here so the failure names the field.
	if key := c.Backup.AccessKey; key != "" && (len(key) < 8 || !garageKeyPattern.MatchString(key)) {
		return fmt.Errorf("backup.access_key must be at least 8 characters of letters, digits, '-', '_' or '.': %q", key)
	}
	if secret := c.Backup.SecretKey; secret != "" && (len(secret) < 16 || !garageSecretPattern.MatchString(secret)) {
		return fmt.Errorf("backup.secret_key must be at least 16 printable ASCII characters without spaces")
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
	// mac_internal/mac_main reach libvirt directly (init-vms) even when `mac`
	// is also set, so validate the raw fields before toSpec collapses them.
	for _, n := range c.configuredNodes() {
		for field, mac := range map[string]string{"mac_internal": n.MACInternal, "mac_main": n.MACMain} {
			if mac != "" && !macPattern.MatchString(mac) {
				return fmt.Errorf("node %s has an invalid %s address: %q", n.Name, field, mac)
			}
		}
	}

	// These fields are interpolated into remote SSH commands, Cobbler
	// registration scripts and awk filters: an unvalidated value breaks or
	// manipulates far-away infrastructure, so their shape must be exact.
	// Cobbler also keys systems by name and MAC, and duplicate addresses make
	// the DHCP host stanzas ambiguous, so reject collisions here.
	seenNames := map[string]string{}
	seenIPs := map[string]string{}
	seenMACs := map[string]string{}
	for _, n := range c.AllNodes() {
		if n.Name != "" {
			if !nodeNamePattern.MatchString(n.Name) {
				return fmt.Errorf("node has an invalid name (letters, digits, dots, underscores, hyphens only): %q", n.Name)
			}
			if prev, ok := seenNames[n.Name]; ok {
				return fmt.Errorf("duplicate node name %q (already used by %s)", n.Name, prev)
			}
			seenNames[n.Name] = n.Name
		}
		if n.IP != "" {
			if ip := net.ParseIP(n.IP); ip == nil || ip.To4() == nil {
				return fmt.Errorf("node %s has an invalid IPv4 address: %q", n.Name, n.IP)
			}
			if prev, ok := seenIPs[n.IP]; ok {
				return fmt.Errorf("node %s reuses IP %s (already used by %s)", n.Name, n.IP, prev)
			}
			seenIPs[n.IP] = n.Name
		}
		if n.FQDN != "" && !fqdnPattern.MatchString(n.FQDN) {
			return fmt.Errorf("node %s has an invalid FQDN (letters, digits, dots, hyphens only): %q", n.Name, n.FQDN)
		}
		if n.MAC != "" {
			if !macPattern.MatchString(n.MAC) {
				return fmt.Errorf("node %s has an invalid MAC address: %q", n.Name, n.MAC)
			}
			key := strings.ToLower(n.MAC)
			if prev, ok := seenMACs[key]; ok {
				return fmt.Errorf("node %s reuses MAC %s (already used by %s)", n.Name, n.MAC, prev)
			}
			seenMACs[key] = n.Name
		}
		// mac_wan is interpolated into an awk filter on the router too.
		if n.MACWAN != "" {
			if !macPattern.MatchString(n.MACWAN) {
				return fmt.Errorf("node %s has an invalid WAN MAC address: %q", n.Name, n.MACWAN)
			}
			key := strings.ToLower(n.MACWAN)
			if prev, ok := seenMACs[key]; ok {
				return fmt.Errorf("node %s reuses MAC %s (already used by %s)", n.Name, n.MACWAN, prev)
			}
			seenMACs[key] = n.Name
		}
	}
	return nil
}

// configuredNodes returns the NodeDetail entries the operator wrote in
// config.yaml. AllNodes additionally synthesises hotdesks, whose MACs are
// generated and have no raw mac_internal/mac_main fields to validate.
func (c *Config) configuredNodes() []NodeDetail {
	nodes := []NodeDetail{c.Nodes.Router, c.Nodes.Jumpstart}
	nodes = append(nodes, c.Nodes.Masters...)
	nodes = append(nodes, c.Nodes.Workers...)
	nodes = append(nodes, c.Nodes.Storage, c.Nodes.Monitor)
	nodes = append(nodes, c.Nodes.LBs...)
	nodes = append(nodes, c.Nodes.CMSFrontends...)
	return nodes
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
var caNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9 ._-]{0,62}[A-Za-z0-9])?$`)

// bridgePattern matches a Linux interface name (IFNAMSIZ is 16 bytes, so at
// most 15 characters) and keeps XML/shell metacharacters out of the network
// definition the init phase renders.
var bridgePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)
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

// garageKeyPattern and garageSecretPattern mirror the constraints Garage's
// Key::import applies to the default access key created from
// GARAGE_DEFAULT_ACCESS_KEY / GARAGE_DEFAULT_SECRET_KEY.
var garageKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var garageSecretPattern = regexp.MustCompile(`^[\x21-\x7E]+$`)

// validUnixUser matches a conservative portable POSIX user name (lowercase
// letters, digits, underscore and hyphen, not starting with a digit or hyphen).
var validUnixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
