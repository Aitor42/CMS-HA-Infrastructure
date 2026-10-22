// Package ansible renders the Ansible provisioning inputs from config.yaml.
//
// config.yaml remains the single source of truth: this package derives the
// inventory (hosts, groups and per-VM variables) and the VM interface layout
// consumed by the provisioning playbook. Ansible is only used to *create and
// declare* infrastructure (networks, pools, domains); runtime operations and
// fencing keep using the thin libvirt/virsh primitive in internal/libvirt.
package ansible

import (
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

// DefaultInventoryPath is where `cms-ha inventory` writes by default.
const DefaultInventoryPath = "ansible/inventory/hosts.yml"

// Interface is one NIC of a VM domain. Network is the libvirt network name
// ("internal", "main", or "default" for the WAN network).
type Interface struct {
	Network string `yaml:"network"`
	MAC     string `yaml:"mac"`
}

// HostVars carries the per-node variables the vm_create role consumes.
type HostVars struct {
	AnsibleHost   string      `yaml:"ansible_host,omitempty"`
	AnsibleUser   string      `yaml:"ansible_user,omitempty"`
	AnsibleKey    string      `yaml:"ansible_ssh_private_key_file,omitempty"`
	AnsibleBecome bool        `yaml:"ansible_become,omitempty"`
	VMName        string      `yaml:"vm_name,omitempty"`
	VMFQDN        string      `yaml:"vm_fqdn,omitempty"`
	VMIPMain      string      `yaml:"vm_ip_main,omitempty"`
	VMInterfaces  []Interface `yaml:"vm_interfaces,omitempty"`
	VMRAMInstall  int         `yaml:"vm_ram_install_mb,omitempty"`
	VMRAMFinal    int         `yaml:"vm_ram_final_mb,omitempty"`
	VMVCPUs       int         `yaml:"vm_vcpus,omitempty"`
	VMDiskGB      int         `yaml:"vm_disk_gb,omitempty"`
	VMDRBDDiskGB  int         `yaml:"vm_drbd_disk_gb,omitempty"`
	// VMCloudInit marks nodes bootstrapped with a cloud-init seed ISO
	// (currently only the jumpstart/PXE server).
	VMCloudInit bool `yaml:"vm_cloud_init,omitempty"`
}

type group struct {
	Hosts    map[string]HostVars `yaml:"hosts,omitempty"`
	Children map[string]*group   `yaml:"children,omitempty"`
	Vars     map[string]any      `yaml:"vars,omitempty"`
}

// Inventory is the top-level document consumed by `ansible-playbook -i`.
type Inventory struct {
	All group `yaml:"all"`
}

// Render builds the Ansible inventory from the configuration.
func Render(cfg *config.Config) *Inventory {
	internal := map[string]HostVars{}
	main := map[string]HostVars{}

	// Internal network nodes.
	internal[cfg.Nodes.Router.Name] = hostVars(cfg, cfg.Nodes.Router, "internal")
	js := hostVars(cfg, cfg.Nodes.Jumpstart, "internal")
	// Jumpstart is bootstrapped from a cloud-init seed ISO (autoinstall); the
	// remaining nodes are PXE-provisioned by Cobbler over the network.
	js.VMCloudInit = true
	internal[cfg.Nodes.Jumpstart.Name] = js
	for _, n := range cfg.Nodes.Masters {
		internal[n.Name] = hostVars(cfg, n, "internal")
	}
	for _, n := range cfg.Nodes.Workers {
		internal[n.Name] = hostVars(cfg, n, "internal")
	}
	internal[cfg.Nodes.Storage.Name] = hostVars(cfg, cfg.Nodes.Storage, "internal")
	internal[cfg.Nodes.Monitor.Name] = hostVars(cfg, cfg.Nodes.Monitor, "internal")

	// Main network nodes.
	for _, n := range cfg.Nodes.LBs {
		main[n.Name] = hostVars(cfg, n, "main")
	}
	for _, n := range cfg.Nodes.CMSFrontends {
		main[n.Name] = hostVars(cfg, n, "main")
	}
	for _, hd := range cfg.HotdeskSpecs() {
		main[hd.Name] = HostVars{
			AnsibleHost:   hd.IP,
			AnsibleUser:   cfg.SSHUser(),
			AnsibleKey:    config.ExpandPath(cfg.SSH.PrivateKey),
			AnsibleBecome: cfg.SSHUser() != "root",
			VMName:        hd.Name,
			VMFQDN:        hd.FQDN,
			VMInterfaces:  []Interface{{Network: "main", MAC: hd.MAC}},
			VMRAMInstall:  hd.RAMInstallMB,
			VMRAMFinal:    hd.RAMFinalMB,
			VMVCPUs:       hd.VCPUs,
			VMDiskGB:      hd.DiskGB,
		}
	}

	allVars := map[string]any{
		"libvirt_uri":      cfg.VM.LibvirtURI,
		"vm_storage_dir":   config.ExpandPath(cfg.VM.StorageDir),
		"ssh_private_key":  config.ExpandPath(cfg.SSH.PrivateKey),
		"internal_cidr":    cfg.Network.Internal.CIDR,
		"internal_gateway": cfg.Network.Internal.Gateway,
		"internal_domain":  cfg.Network.Internal.Domain,
		"internal_bridge":  cfg.Network.Internal.Bridge,
		"main_cidr":        cfg.Network.Main.CIDR,
		"main_gateway":     cfg.Network.Main.Gateway,
		"main_domain":      cfg.Network.Main.Domain,
		"main_bridge":      cfg.Network.Main.Bridge,
		"lb_vip":           cfg.LBVIP(),
	}

	return &Inventory{
		All: group{
			Vars: allVars,
			Children: map[string]*group{
				// The hypervisor is where libvirt runs; the provisioning
				// playbook targets it locally.
				"hypervisor": {
					Hosts: map[string]HostVars{"localhost": {AnsibleHost: "127.0.0.1"}},
					Vars: map[string]any{
						"ansible_connection":         "local",
						"ansible_python_interpreter": "/usr/bin/python3",
					},
				},
				// Every VM, grouped by the network it primarily lives on.
				"vms": {
					Children: map[string]*group{
						"internal": {Hosts: internal},
						"main":     {Hosts: main},
					},
				},
				// Role groups: bare host names, no variables, so the per-host
				// variables defined above are never overridden.
				"k3s_servers":    {Hosts: bareNames(cfg.Nodes.Masters)},
				"k3s_agents":     {Hosts: bareNames(cfg.Nodes.Workers)},
				"load_balancers": {Hosts: bareNames(cfg.Nodes.LBs)},
				"cms_frontends":  {Hosts: bareNames(cfg.Nodes.CMSFrontends)},
			},
		},
	}
}

// Marshal renders the inventory as deterministic YAML with a header comment.
func Marshal(cfg *config.Config) ([]byte, error) {
	body, err := yaml.Marshal(Render(cfg))
	if err != nil {
		return nil, fmt.Errorf("marshal ansible inventory: %w", err)
	}
	header := "# GENERATED FILE — do not edit by hand.\n" +
		"# Produced by `cms-ha inventory` from config.yaml (single source of truth).\n"
	return append([]byte(header), body...), nil
}

// hostVars builds the per-host variable set. primary selects which address is
// used as ansible_host for dual-homed nodes.
func hostVars(cfg *config.Config, n config.NodeDetail, primary string) HostVars {
	ip := n.IP
	if ip == "" && primary == "main" {
		ip = n.IPMain
	}
	return HostVars{
		AnsibleHost:   ip,
		AnsibleUser:   cfg.SSHUser(),
		AnsibleKey:    config.ExpandPath(cfg.SSH.PrivateKey),
		AnsibleBecome: cfg.SSHUser() != "root",
		VMName:        n.Name,
		VMFQDN:        n.FQDN,
		VMIPMain:      n.IPMain,
		VMInterfaces:  interfacesFor(n, primary),
		VMRAMInstall:  n.RAMInstallMB,
		VMRAMFinal:    n.RAMFinalMB,
		VMVCPUs:       n.VCPUs,
		VMDiskGB:      n.DiskGB,
		VMDRBDDiskGB:  n.DRBDDiskGB,
	}
}

// interfacesFor maps the node's MAC fields to libvirt networks. Dual-homed
// nodes (router, jumpstart) expose the internal/main/WAN MACs; single-homed
// nodes use the MAC field on their primary network.
func interfacesFor(n config.NodeDetail, primary string) []Interface {
	var ifs []Interface
	if n.MACInternal != "" {
		ifs = append(ifs, Interface{Network: "internal", MAC: n.MACInternal})
	}
	if n.MACMain != "" {
		ifs = append(ifs, Interface{Network: "main", MAC: n.MACMain})
	}
	if n.MACWAN != "" {
		ifs = append(ifs, Interface{Network: "default", MAC: n.MACWAN})
	}
	if len(ifs) == 0 && n.MAC != "" {
		ifs = append(ifs, Interface{Network: primary, MAC: n.MAC})
	}
	return ifs
}

// bareNames returns role-group entries (host names with no variables).
func bareNames(nodes []config.NodeDetail) map[string]HostVars {
	m := make(map[string]HostVars, len(nodes))
	for _, n := range nodes {
		if n.Name != "" {
			m[n.Name] = HostVars{}
		}
	}
	return m
}

// ResolvePath resolves a (possibly relative) inventory path against root.
func ResolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}
