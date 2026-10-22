package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ExpandPath expands environment variables and tilde (~) prefix in file paths.
func ExpandPath(path string) string {
	if path == "" {
		return ""
	}
	path = os.ExpandEnv(path)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	return filepath.Clean(path)
}

// Config represents the top-level configuration.
type Config struct {
	VM         VMConfig         `yaml:"vm"`
	SSH        SSHConfig        `yaml:"ssh"`
	Network    NetworkConfig    `yaml:"network"`
	Nodes      NodesConfig      `yaml:"nodes"`
	Database   DatabaseConfig   `yaml:"database"`
	WordPress  WordPressConfig  `yaml:"wordpress"`
	Monitoring MonitoringConfig `yaml:"monitoring"`
	PKI        PKIConfig        `yaml:"pki"`
	Deploy     DeployConfig     `yaml:"deploy"`
	DRBD       DRBDConfig       `yaml:"drbd"`
	Backup     BackupConfig     `yaml:"backup"`
	Traffic    TrafficConfig    `yaml:"traffic"`
}

type VMConfig struct {
	StorageDir string `yaml:"storage_dir"`
	LibvirtURI string `yaml:"libvirt_uri"`
}

type SSHConfig struct {
	// User is the non-root account the orchestrator logs in as. Privileged
	// commands are elevated with `sudo -n`; empty falls back to "root" for
	// backwards compatibility.
	User string `yaml:"user"`
	// KnownHostsFile is the TOFU database used to pin VM host keys.
	KnownHostsFile string        `yaml:"known_hosts_file"`
	PrivateKey     string        `yaml:"private_key"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
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

type NetworkConfig struct {
	Internal NetworkDetail `yaml:"internal"`
	Main     NetworkDetail `yaml:"main"`
}

type NetworkDetail struct {
	CIDR    string `yaml:"cidr"`
	Gateway string `yaml:"gateway"`
	Domain  string `yaml:"domain"`
	Bridge  string `yaml:"bridge"`
	// VIP is the keepalived floating address (main network only): the address
	// clients use and the router DNATs to. Empty on the internal network.
	VIP string `yaml:"vip"`
}

type NodesConfig struct {
	Router       NodeDetail   `yaml:"router"`
	Jumpstart    NodeDetail   `yaml:"jumpstart"`
	Masters      []NodeDetail `yaml:"masters"`
	Workers      []NodeDetail `yaml:"workers"`
	Storage      NodeDetail   `yaml:"storage"`
	Monitor      NodeDetail   `yaml:"monitor"`
	LBs          []NodeDetail `yaml:"lbs"`
	CMSFrontends []NodeDetail `yaml:"cms_frontends"`
	Hotdesks     HotdesksConf `yaml:"hotdesks"`
}

type NodeDetail struct {
	Name         string `yaml:"name"`
	IP           string `yaml:"ip"`
	IPMain       string `yaml:"ip_main"`
	FQDN         string `yaml:"fqdn"`
	MAC          string `yaml:"mac"`
	MACInternal  string `yaml:"mac_internal"`
	MACMain      string `yaml:"mac_main"`
	MACWAN       string `yaml:"mac_wan"`
	RAMInstallMB int    `yaml:"ram_install_mb"`
	RAMFinalMB   int    `yaml:"ram_final_mb"`
	VCPUs        int    `yaml:"vcpus"`
	DiskGB       int    `yaml:"disk_gb"`
	DRBDDiskGB   int    `yaml:"drbd_disk_gb"`
}

type HotdesksConf struct {
	Count        int `yaml:"count"`
	Max          int `yaml:"max"`
	RAMInstallMB int `yaml:"ram_install_mb"`
	RAMFinalMB   int `yaml:"ram_final_mb"`
	VCPUs        int `yaml:"vcpus"`
	DiskGB       int `yaml:"disk_gb"`
	BaseIPOctet  int `yaml:"base_ip_octet"`
}

type DatabaseConfig struct {
	Name             string `yaml:"name"`
	User             string `yaml:"user"`
	Password         string `yaml:"password"`
	RootPassword     string `yaml:"root_password"`
	ExporterPassword string `yaml:"exporter_password"`
	Port             int    `yaml:"port"`
}

type WordPressConfig struct {
	AdminPassword string `yaml:"admin_password"`
}

type MonitoringConfig struct {
	Exporters ExportersConfig `yaml:"exporters"`
}

type ExportersConfig struct {
	NginxVersion  string `yaml:"nginx_version"`
	ApacheVersion string `yaml:"apache_version"`
	MysqldVersion string `yaml:"mysqld_version"`
}

type PKIConfig struct {
	CAPort              int    `yaml:"ca_port"`
	ProvisionerPassword string `yaml:"provisioner_password"`
	StepVersion         string `yaml:"step_version"`
	StepCAVersion       string `yaml:"step_ca_version"`
	Domain              string `yaml:"domain"`
	// CAName is the CA's common name (used by the verify phase to confirm the
	// load-balancer certificate was issued by the internal CA, not self-signed).
	CAName string `yaml:"ca_name"`
	// CertDNSNames are the configured DNS SANs for the load-balancer
	// certificate; when empty, LBTLSDNSNames falls back to a built-in default.
	CertDNSNames []string `yaml:"cert_dns_names"`
	// CertMinValidDays is the minimum remaining validity the verify phase
	// requires on the LB certificate (renewal keeps it comfortably above it).
	CertMinValidDays int `yaml:"cert_min_valid_days"`
	// CertLifetimeHours is the requested lifetime of the LB certificate. It
	// must exceed CertMinValidDays so the certificate is not "expired" the
	// moment it is issued.
	CertLifetimeHours int `yaml:"cert_lifetime_hours"`
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

type DeployConfig struct {
	SSHTimeout   time.Duration `yaml:"ssh_timeout"`
	StaggerDelay time.Duration `yaml:"stagger_delay"`
	// NodeInstallTimeout bounds how long one batch of client nodes may take to
	// finish installing and answer SSH. The nodes of a batch install at the
	// same time and share the host's vCPUs, so a host with fewer cores than
	// the reference lab needs a larger budget than the 15m default.
	NodeInstallTimeout time.Duration `yaml:"node_install_timeout"`
	// InstallBatchSize caps how many guests install at the same time. Zero
	// keeps the documented semantic batches (network and management, then the
	// Kubernetes nodes, then the frontends); a positive value re-splits that
	// same order into chunks of at most this many guests.
	//
	// It is both a memory and a speed setting. Each installer saturates about
	// one vCPU, so the width that minimises wall time is the host's vCPU
	// count: anything above it is a second round of installs, and anything
	// below it leaves cores idle for no benefit.
	InstallBatchSize int `yaml:"install_batch_size"`
}

// DRBDConfig controls the DRBD cms_data resource, in particular STONITH
// fencing: before a node promotes itself it must power off the failed peer at
// the hypervisor so two nodes can never write the replicated volume.
type DRBDConfig struct {
	// Fencing requires a confirmed power-off of the peer before promoting.
	// When false the watchdog falls back to the legacy best-effort ICMP
	// heuristic, which cannot rule out split-brain; only use it for demos.
	Fencing bool `yaml:"fencing"`
	// FenceCommand powers off the peer. FenceStateCommand queries its state.
	// Both are shell snippets in which the literal {domain} is replaced with
	// the peer's libvirt domain name. Leave empty to auto-derive an SSH+virsh
	// command against the hypervisor (FenceCommands does this).
	FenceCommand      string `yaml:"fence_command"`
	FenceStateCommand string `yaml:"fence_state_command"`
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

// BackupConfig controls the off-DRBD backup target: an S3-compatible object
// store (Garage) running as a container on the hypervisor, on a data volume
// that is NOT the DRBD device, plus the daily dump/upload and the restore
// drill. Engine "" disables the whole backup feature.
type BackupConfig struct {
	// Engine selects the object-store implementation. Only "garage" is
	// supported today; "" disables backup provisioning entirely.
	Engine string `yaml:"engine"`
	// GarageVersion is the pinned image tag (e.g. "v2.4.1").
	GarageVersion string `yaml:"garage_version"`
	// DataDir is the hypervisor directory holding Garage data/metadata. It must
	// live outside the DRBD device so a split-brain cannot take the copies.
	DataDir string `yaml:"data_dir"`
	// S3Port is the S3 API port exposed on the hypervisor's internal address.
	S3Port int `yaml:"s3_port"`
	// S3Region is advertised to S3 clients (any value works with Garage).
	S3Region string `yaml:"s3_region"`
	// Bucket receives the dumps.
	Bucket string `yaml:"bucket"`
	// AccessKey is the S3 access key id (not secret on its own).
	AccessKey string `yaml:"access_key"`
	// SecretKey is the S3 secret; store it age-encrypted like other secrets.
	SecretKey string `yaml:"secret_key"`
	// RetentionDays is how long objects are kept in the bucket and on the
	// local staging volume.
	RetentionDays int `yaml:"retention_days"`
	// Schedule is the CronJob schedule for the dump+upload.
	Schedule string `yaml:"schedule"`
	// RestoreTestSchedule is the CronJob schedule for the restore drill.
	RestoreTestSchedule string `yaml:"restore_test_schedule"`
}

// TrafficConfig configures the k6 load/smoke generator.
type TrafficConfig struct {
	// K6Version is the pinned grafana/k6 image tag (no floating tags).
	K6Version string `yaml:"k6_version"`
	// PrometheusRWURL, when set, streams k6 metrics to Prometheus through its
	// remote-write endpoint (e.g. http://192.168.10.20:9090/api/v1/write).
	PrometheusRWURL string `yaml:"prometheus_rw_url"`
	// VHost is the HTTP Host header the CMS vhost expects.
	VHost string `yaml:"vhost"`
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

// NodeSpec is a flattened representation of a node
type NodeSpec struct {
	Name         string
	IP           string
	FQDN         string
	MAC          string
	MACWAN       string
	RAMInstallMB int
	RAMFinalMB   int
	VCPUs        int
	DiskGB       int
}

// Load loads the configuration from the given path using default age key location.
func Load(path string) (*Config, error) {
	return LoadWithKey(path, "")
}

// LoadWithKey loads configuration from the given path using a specified age key file.
func LoadWithKey(path string, keyPath string) (*Config, error) {
	path = ExpandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	c.VM.StorageDir = ExpandPath(c.VM.StorageDir)
	c.SSH.PrivateKey = ExpandPath(c.SSH.PrivateKey)

	// Allow NUM_HOTDESKS environment variable to override hotdesks count dynamically (matching scripts/config.sh)
	if envHotdesks := os.Getenv("NUM_HOTDESKS"); envHotdesks != "" {
		if val, err := strconv.Atoi(envHotdesks); err == nil && val > 0 {
			c.Nodes.Hotdesks.Count = val
		} else {
			slog.Warn("ignoring invalid NUM_HOTDESKS value", "value", envHotdesks)
		}
	}

	resolvedKeyPath := keyPath
	if resolvedKeyPath == "" {
		defaultKey := ExpandPath("${HOME}/.config/cms-ha/age.key")
		if _, err := os.Stat(defaultKey); err == nil {
			resolvedKeyPath = defaultKey
		} else if _, err := os.Stat("private.key"); err == nil {
			resolvedKeyPath = "private.key"
		} else {
			resolvedKeyPath = defaultKey
		}
	} else {
		resolvedKeyPath = ExpandPath(resolvedKeyPath)
	}

	hasEncrypted := IsEncrypted(c.Database.Password) ||
		IsEncrypted(c.Database.RootPassword) ||
		IsEncrypted(c.Database.ExporterPassword) ||
		IsEncrypted(c.WordPress.AdminPassword) ||
		IsEncrypted(c.PKI.ProvisionerPassword) ||
		IsEncrypted(c.Backup.SecretKey)

	if hasEncrypted {
		if _, err := os.Stat(resolvedKeyPath); err != nil {
			return nil, fmt.Errorf("configuration contains encrypted fields but key file %s does not exist: %w", resolvedKeyPath, err)
		}

		d, err := newDecrypter(resolvedKeyPath)
		if err != nil {
			return nil, err
		}
		secrets := []struct {
			value *string
			label string
		}{
			{&c.Database.Password, "db password"},
			{&c.Database.RootPassword, "db root password"},
			{&c.Database.ExporterPassword, "db exporter password"},
			{&c.WordPress.AdminPassword, "WordPress admin password"},
			{&c.PKI.ProvisionerPassword, "pki provisioner password"},
			{&c.Backup.SecretKey, "backup secret key"},
		}
		for _, s := range secrets {
			if !IsEncrypted(*s.value) {
				continue
			}
			v, err := d.decrypt(*s.value)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s: %w", s.label, err)
			}
			*s.value = v
		}
	}

	return &c, nil
}

// Validate performs structural and semantic checks on the configuration.
func (c *Config) Validate() error {
	if c.VM.StorageDir == "" {
		return fmt.Errorf("vm.storage_dir is required")
	}
	if c.SSH.User != "" && !validUnixUser.MatchString(c.SSH.User) {
		return fmt.Errorf("ssh.user is not a valid Unix user name: %q", c.SSH.User)
	}
	if c.Deploy.InstallBatchSize < 0 {
		return fmt.Errorf("deploy.install_batch_size must not be negative (got %d)", c.Deploy.InstallBatchSize)
	}
	if _, _, err := net.ParseCIDR(c.Network.Internal.CIDR); err != nil {
		return fmt.Errorf("network.internal.cidr is not a valid CIDR: %w", err)
	}
	if _, _, err := net.ParseCIDR(c.Network.Main.CIDR); err != nil {
		return fmt.Errorf("network.main.cidr is not a valid CIDR: %w", err)
	}
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
	if len(c.Nodes.CMSFrontends) < 1 {
		return fmt.Errorf("at least 1 CMS frontend node is required")
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
	if v := strings.TrimSpace(c.Traffic.K6Version); v == "latest" {
		return fmt.Errorf("traffic.k6_version must be pinned to a specific tag (got %q)", v)
	}
	if rw := strings.TrimSpace(c.Traffic.PrometheusRWURL); rw != "" {
		if u, err := url.Parse(rw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("traffic.prometheus_rw_url must be an http(s) URL: %q", rw)
		}
	}

	// Hotdesk IPs are generated as <network>.<octet>: detect overflow before
	// the node sweep below so this clearer message wins (generation happens
	// inside AllNodes and would otherwise surface as "invalid IPv4 .256").
	if c.Nodes.Hotdesks.Count > 0 {
		base := c.Nodes.Hotdesks.BaseIPOctet
		if base == 0 || base == 201 {
			base = 201
		}
		if base+c.Nodes.Hotdesks.Count-1 > 254 {
			return fmt.Errorf("hotdesk range overflows: base octet %d with %d nodes exceeds 254", base, c.Nodes.Hotdesks.Count)
		}
	}

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

// validUnixUser matches a conservative portable POSIX user name (lowercase
// letters, digits, underscore and hyphen, not starting with a digit or hyphen).
var validUnixUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func toSpec(n NodeDetail, fallbackIP string) NodeSpec {
	ip := n.IP
	if ip == "" {
		ip = fallbackIP
	}
	mac := n.MAC
	if mac == "" && n.MACInternal != "" {
		mac = n.MACInternal
	} else if mac == "" && n.MACMain != "" {
		mac = n.MACMain
	}
	return NodeSpec{
		Name:         n.Name,
		IP:           ip,
		FQDN:         n.FQDN,
		MAC:          mac,
		MACWAN:       n.MACWAN,
		RAMInstallMB: n.RAMInstallMB,
		RAMFinalMB:   n.RAMFinalMB,
		VCPUs:        n.VCPUs,
		DiskGB:       n.DiskGB,
	}
}

// InternalNodes returns the nodes on the internal network.
func (c *Config) InternalNodes() []NodeSpec {
	var nodes []NodeSpec
	nodes = append(nodes, toSpec(c.Nodes.Router, ""))
	nodes = append(nodes, toSpec(c.Nodes.Jumpstart, c.Nodes.Jumpstart.IP))
	for _, n := range c.Nodes.Masters {
		nodes = append(nodes, toSpec(n, ""))
	}
	for _, n := range c.Nodes.Workers {
		nodes = append(nodes, toSpec(n, ""))
	}
	nodes = append(nodes, toSpec(c.Nodes.Storage, ""))
	nodes = append(nodes, toSpec(c.Nodes.Monitor, ""))
	return nodes
}

// MainNodes returns the nodes on the main network.
func (c *Config) MainNodes() []NodeSpec {
	var nodes []NodeSpec
	if c.Nodes.Jumpstart.IPMain != "" {
		js := toSpec(c.Nodes.Jumpstart, c.Nodes.Jumpstart.IPMain)
		js.MAC = c.Nodes.Jumpstart.MACMain
		nodes = append(nodes, js)
	}
	for _, lb := range c.Nodes.LBs {
		nodes = append(nodes, toSpec(lb, ""))
	}
	for _, n := range c.Nodes.CMSFrontends {
		nodes = append(nodes, toSpec(n, ""))
	}
	nodes = append(nodes, c.HotdeskSpecs()...)
	return nodes
}

// HotdeskSpecs synthesises the hotdesk nodes from Nodes.Hotdesks.Count and
// BaseIPOctet.
func (c *Config) HotdeskSpecs() []NodeSpec {
	// Hotdesks live on the main network, so derive their address range from the
	// configured CIDR instead of hardcoding 192.168.20.0/24.
	prefix := PrefixFromCIDR(c.Network.Main.CIDR, "192.168.20.")
	var nodes []NodeSpec
	for i := 1; i <= c.Nodes.Hotdesks.Count; i++ {
		lastOctet := 200 + i
		if c.Nodes.Hotdesks.BaseIPOctet > 0 && c.Nodes.Hotdesks.BaseIPOctet != 201 {
			lastOctet = c.Nodes.Hotdesks.BaseIPOctet + i - 1
		}
		nodes = append(nodes, NodeSpec{
			Name:         fmt.Sprintf("main-hotdesk%d", i),
			IP:           fmt.Sprintf("%s%d", prefix, lastOctet),
			FQDN:         fmt.Sprintf("main-hotdesk%d.main.local", i),
			MAC:          fmt.Sprintf("52:54:00:10:02:%02x", lastOctet),
			RAMInstallMB: c.Nodes.Hotdesks.RAMInstallMB,
			RAMFinalMB:   c.Nodes.Hotdesks.RAMFinalMB,
			VCPUs:        c.Nodes.Hotdesks.VCPUs,
			DiskGB:       c.Nodes.Hotdesks.DiskGB,
		})
	}
	return nodes
}

// PrefixFromCIDR returns the first three octets of an IPv4 CIDR plus the
// trailing dot (e.g. "192.168.20."), so callers can compose static host IPs
// in that range. Returns fallback for malformed CIDRs.
func PrefixFromCIDR(cidr, fallback string) string {
	parts := strings.Split(cidr, ".")
	if len(parts) >= 3 {
		return fmt.Sprintf("%s.%s.%s.", parts[0], parts[1], parts[2])
	}
	return fallback
}

// PrimaryLB returns the first load balancer node (the keepalived MASTER
// candidate). Returns a zero NodeDetail when none is configured.
func (c *Config) PrimaryLB() NodeDetail {
	if len(c.Nodes.LBs) == 0 {
		return NodeDetail{}
	}
	return c.Nodes.LBs[0]
}

// LBVIP returns the keepalived floating address clients (and the router DNAT)
// use to reach the CMS.
func (c *Config) LBVIP() string {
	return c.Network.Main.VIP
}

// AllNodes returns all nodes across both networks
func (c *Config) AllNodes() []NodeSpec {
	var all []NodeSpec
	all = append(all, c.InternalNodes()...)
	// Don't duplicate Jumpstart
	for _, n := range c.MainNodes() {
		if n.Name != c.Nodes.Jumpstart.Name {
			all = append(all, n)
		}
	}
	return all
}

// AllNodeIPs returns every configured node IP, plus the jumpstart's
// main-network address.
func (c *Config) AllNodeIPs() []string {
	var ips []string
	for _, n := range c.AllNodes() {
		if n.IP != "" {
			ips = append(ips, n.IP)
		}
	}
	if c.Nodes.Jumpstart.IPMain != "" {
		ips = append(ips, c.Nodes.Jumpstart.IPMain)
	}
	return ips
}
