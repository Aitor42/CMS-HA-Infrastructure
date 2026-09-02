package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
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
	VM         VMConfig         `mapstructure:"vm"`
	SSH        SSHConfig        `mapstructure:"ssh"`
	Network    NetworkConfig    `mapstructure:"network"`
	Nodes      NodesConfig      `mapstructure:"nodes"`
	Database   DatabaseConfig   `mapstructure:"database"`
	Monitoring MonitoringConfig `mapstructure:"monitoring"`
	PKI        PKIConfig        `mapstructure:"pki"`
	Deploy     DeployConfig     `mapstructure:"deploy"`
}

type VMConfig struct {
	StorageDir string `mapstructure:"storage_dir"`
	LibvirtURI string `mapstructure:"libvirt_uri"`
}

type SSHConfig struct {
	PrivateKey     string        `mapstructure:"private_key"`
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
	BatchMode      bool          `mapstructure:"batch_mode"`
}

type NetworkConfig struct {
	Internal NetworkDetail `mapstructure:"internal"`
	Main     NetworkDetail `mapstructure:"main"`
}

type NetworkDetail struct {
	CIDR    string `mapstructure:"cidr"`
	Gateway string `mapstructure:"gateway"`
	Domain  string `mapstructure:"domain"`
	Bridge  string `mapstructure:"bridge"`
}

type NodesConfig struct {
	Router       NodeDetail   `mapstructure:"router"`
	Jumpstart    NodeDetail   `mapstructure:"jumpstart"`
	Masters      []NodeDetail `mapstructure:"masters"`
	Workers      []NodeDetail `mapstructure:"workers"`
	Storage      NodeDetail   `mapstructure:"storage"`
	Monitor      NodeDetail   `mapstructure:"monitor"`
	LB           NodeDetail   `mapstructure:"lb"`
	CMSFrontends []NodeDetail `mapstructure:"cms_frontends"`
	Hotdesks     HotdesksConf `mapstructure:"hotdesks"`
}

type NodeDetail struct {
	Name         string `mapstructure:"name"`
	IP           string `mapstructure:"ip"`
	IPMain       string `mapstructure:"ip_main"`
	FQDN         string `mapstructure:"fqdn"`
	MAC          string `mapstructure:"mac"`
	MACInternal  string `mapstructure:"mac_internal"`
	MACMain      string `mapstructure:"mac_main"`
	MACWAN       string `mapstructure:"mac_wan"`
	RAMInstallMB int    `mapstructure:"ram_install_mb"`
	RAMFinalMB   int    `mapstructure:"ram_final_mb"`
	VCPUs        int    `mapstructure:"vcpus"`
	DiskGB       int    `mapstructure:"disk_gb"`
	DRBDDiskGB   int    `mapstructure:"drbd_disk_gb"`
}

type HotdesksConf struct {
	Count        int `mapstructure:"count"`
	Max          int `mapstructure:"max"`
	RAMInstallMB int `mapstructure:"ram_install_mb"`
	RAMFinalMB   int `mapstructure:"ram_final_mb"`
	VCPUs        int `mapstructure:"vcpus"`
	DiskGB       int `mapstructure:"disk_gb"`
	BaseIPOctet  int `mapstructure:"base_ip_octet"`
}

type DatabaseConfig struct {
	Name         string `mapstructure:"name"`
	User         string `mapstructure:"user"`
	Password     string `mapstructure:"password"`
	RootPassword string `mapstructure:"root_password"`
	Port         int    `mapstructure:"port"`
}

type MonitoringConfig struct {
	Exporters ExportersConfig `mapstructure:"exporters"`
}

type ExportersConfig struct {
	NginxVersion  string `mapstructure:"nginx_version"`
	ApacheVersion string `mapstructure:"apache_version"`
	MysqldVersion string `mapstructure:"mysqld_version"`
}

type PKIConfig struct {
	CAPort              int    `mapstructure:"ca_port"`
	ProvisionerPassword string `mapstructure:"provisioner_password"`
	StepVersion         string `mapstructure:"step_version"`
	StepCAVersion       string `mapstructure:"step_ca_version"`
	Domain              string `mapstructure:"domain"`
}

type DeployConfig struct {
	SSHTimeout   time.Duration `mapstructure:"ssh_timeout"`
	StaggerDelay time.Duration `mapstructure:"stagger_delay"`
}

// NodeSpec is a flattened representation of a node
type NodeSpec struct {
	Name         string
	IP           string
	FQDN         string
	MAC          string
	RAMInstallMB int
	RAMFinalMB   int
	VCPUs        int
	DiskGB       int
	DRBDDiskGB   int
}

// Load loads the configuration from the given path.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	
	c.VM.StorageDir = ExpandPath(c.VM.StorageDir)
	c.SSH.PrivateKey = ExpandPath(c.SSH.PrivateKey)
	
	defaultKeyPath := ExpandPath("${HOME}/.config/cms-ha/age.key")
	if _, err := os.Stat(defaultKeyPath); err == nil {
		var err error
		if IsEncrypted(c.Database.Password) {
			c.Database.Password, err = DecryptValue(c.Database.Password, defaultKeyPath)
			if err != nil {
				return nil, fmt.Errorf("decrypt db password: %w", err)
			}
		}
		if IsEncrypted(c.Database.RootPassword) {
			c.Database.RootPassword, err = DecryptValue(c.Database.RootPassword, defaultKeyPath)
			if err != nil {
				return nil, fmt.Errorf("decrypt db root password: %w", err)
			}
		}
		if IsEncrypted(c.PKI.ProvisionerPassword) {
			c.PKI.ProvisionerPassword, err = DecryptValue(c.PKI.ProvisionerPassword, defaultKeyPath)
			if err != nil {
				return nil, fmt.Errorf("decrypt pki provisioner password: %w", err)
			}
		}
	}
	
	return &c, nil
}

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
		RAMInstallMB: n.RAMInstallMB,
		RAMFinalMB:   n.RAMFinalMB,
		VCPUs:        n.VCPUs,
		DiskGB:       n.DiskGB,
		DRBDDiskGB:   n.DRBDDiskGB,
	}
}

// InternalNodes returns nodes on 192.168.10.0/24
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

// MainNodes returns nodes on 192.168.20.0/24
func (c *Config) MainNodes() []NodeSpec {
	var nodes []NodeSpec
	if c.Nodes.Jumpstart.IPMain != "" {
		js := toSpec(c.Nodes.Jumpstart, c.Nodes.Jumpstart.IPMain)
		js.MAC = c.Nodes.Jumpstart.MACMain
		nodes = append(nodes, js)
	}
	nodes = append(nodes, toSpec(c.Nodes.LB, ""))
	for _, n := range c.Nodes.CMSFrontends {
		nodes = append(nodes, toSpec(n, ""))
	}
	nodes = append(nodes, c.HotdeskSpecs()...)
	return nodes
}

// HotdeskSpecs generates dynamic hotdesk specs
func (c *Config) HotdeskSpecs() []NodeSpec {
	var nodes []NodeSpec
	for i := 1; i <= c.Nodes.Hotdesks.Count; i++ {
		nodes = append(nodes, NodeSpec{
			Name:         fmt.Sprintf("hotdesk%d", i),
			IP:           fmt.Sprintf("192.168.20.%d", c.Nodes.Hotdesks.BaseIPOctet+i),
			FQDN:         fmt.Sprintf("hotdesk%d.main.local", i),
			MAC:          fmt.Sprintf("52:54:00:10:02:%02x", 100+i), // generated mac
			RAMInstallMB: c.Nodes.Hotdesks.RAMInstallMB,
			RAMFinalMB:   c.Nodes.Hotdesks.RAMFinalMB,
			VCPUs:        c.Nodes.Hotdesks.VCPUs,
			DiskGB:       c.Nodes.Hotdesks.DiskGB,
		})
	}
	return nodes
}

// AllNodes returns a flat list of all nodes
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

// AllNodeIPs returns all IPs
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

// SSHOptions returns SSH option flags
func (c *Config) SSHOptions() []string {
	opts := []string{
		"-i", c.SSH.PrivateKey,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(c.SSH.ConnectTimeout.Seconds())),
	}
	if c.SSH.BatchMode {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	return opts
}
