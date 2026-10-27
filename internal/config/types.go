package config

import "time"

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

// VMConfig holds the storage directory and libvirt URI for the lab VMs.
type VMConfig struct {
	StorageDir string `yaml:"storage_dir"`
	LibvirtURI string `yaml:"libvirt_uri"`
	// JumpstartImage selects how the jumpstart node is installed. "iso" (the
	// default) boots the Ubuntu server ISO through the same autoinstall path
	// the lab and the Ansible roles use. "cloud" is the opt-in fast path: it
	// boots a qcow2 overlay of the official cloud image and lets cloud-init
	// configure it, which is useful for iteration and CI but does not replace
	// the PXE/ISO deliverable.
	JumpstartImage string `yaml:"jumpstart_image"`
	// JumpstartBaseImage is the cloud image used when JumpstartImage is
	// "cloud". Empty looks for noble-server-cloudimg-amd64.img (or the legacy
	// ubuntu-24.04-server-cloudimg-amd64.img name) in StorageDir.
	JumpstartBaseImage string `yaml:"jumpstart_base_image"`
	// Provisioning selects how client nodes get their base OS. "pxe" (the
	// default) keeps the Cobbler deliverable path; "cloud" boots a qcow2
	// overlay of JumpstartBaseImage with per-node cloud-init and is the
	// opt-in fast path for iteration and CI.
	Provisioning string `yaml:"provisioning"`
}

// SSHConfig controls the deploy account, key and host-key verification.
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

// NetworkConfig describes the internal and main lab networks.
type NetworkConfig struct {
	Internal NetworkDetail `yaml:"internal"`
	Main     NetworkDetail `yaml:"main"`
}

// NetworkDetail is one libvirt network and its addressing.
type NetworkDetail struct {
	CIDR    string `yaml:"cidr"`
	Gateway string `yaml:"gateway"`
	Domain  string `yaml:"domain"`
	Bridge  string `yaml:"bridge"`
	// VIP is the keepalived floating address (main network only): the address
	// clients use and the router DNATs to. Empty on the internal network.
	VIP string `yaml:"vip"`
}

// NodesConfig lists every node in the topology.
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

// NodeDetail describes one VM's addresses, MACs and resources.
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

// HotdesksConf controls the optional hotdesk cluster.
type HotdesksConf struct {
	Count        int `yaml:"count"`
	Max          int `yaml:"max"`
	RAMInstallMB int `yaml:"ram_install_mb"`
	RAMFinalMB   int `yaml:"ram_final_mb"`
	VCPUs        int `yaml:"vcpus"`
	DiskGB       int `yaml:"disk_gb"`
	BaseIPOctet  int `yaml:"base_ip_octet"`
}

// DatabaseConfig holds the MariaDB credentials and database name.
type DatabaseConfig struct {
	Name             string `yaml:"name"`
	User             string `yaml:"user"`
	Password         string `yaml:"password"`
	RootPassword     string `yaml:"root_password"`
	ExporterPassword string `yaml:"exporter_password"`
	Port             int    `yaml:"port"`
}

// WordPressConfig holds the WordPress admin credentials.
type WordPressConfig struct {
	AdminPassword string `yaml:"admin_password"`
}

// MonitoringConfig controls the monitoring exporters and the Grafana admin
// credential. An empty GrafanaAdminPassword makes the monitoring phase
// generate a per-deployment secret and reset the default admin account.
type MonitoringConfig struct {
	Exporters            ExportersConfig `yaml:"exporters"`
	GrafanaAdminPassword string          `yaml:"grafana_admin_password"`
}

// ExportersConfig pins the exporter versions used by the monitor.
type ExportersConfig struct {
	NginxVersion  string `yaml:"nginx_version"`
	ApacheVersion string `yaml:"apache_version"`
	MysqldVersion string `yaml:"mysqld_version"`
}

// PKIConfig configures the internal step-ca and load-balancer certificate.
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

// DeployConfig controls deployment timing and installer batching.
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
	// PuppetConcurrency caps how many nodes run their first Puppet catalog at
	// the same time. Zero (the default) derives it from the host's CPU count
	// (4 base, at most 8), and the PUPPET_CONCURRENCY environment variable
	// overrides both. Each agent spends most of its catalog applying resources
	// inside its own guest, so raising it mostly parallelises guest work while
	// the puppetserver serialises the compile step.
	PuppetConcurrency int `yaml:"puppet_concurrency"`
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
