package tests

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/ansible"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
)

// invGroup mirrors the subset of the Ansible YAML inventory the tests inspect.
type invGroup struct {
	Hosts    map[string]map[string]any `yaml:"hosts"`
	Children map[string]*invGroup      `yaml:"children"`
	Vars     map[string]any            `yaml:"vars"`
}

type invDoc struct {
	All invGroup `yaml:"all"`
}

// collectHosts flattens the hosts of a group and its descendants.
func collectHosts(g *invGroup, out map[string]map[string]any) {
	if g == nil {
		return
	}
	for name, vars := range g.Hosts {
		out[name] = vars
	}
	for _, child := range g.Children {
		collectHosts(child, out)
	}
}

// TestAnsibleInventoryParityWithConfig: the Ansible inventory must be in
// 100% parity with config.yaml (names + IPs).
func TestAnsibleInventoryParityWithConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}

	data, err := ansible.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}

	var doc invDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("inventory is not valid YAML: %v", err)
	}

	vms := doc.All.Children["vms"]
	if vms == nil {
		t.Fatal("inventory has no 'vms' group")
	}
	got := map[string]map[string]any{}
	collectHosts(vms, got)

	// Every configured node must appear exactly once, with its IP.
	expected := map[string]string{}
	for _, n := range cfg.AllNodes() {
		if n.Name == "" {
			continue
		}
		expected[n.Name] = n.IP
	}

	if len(got) != len(expected) {
		t.Errorf("inventory/config parity: got %d hosts, want %d", len(got), len(expected))
	}
	for name, ip := range expected {
		hv, ok := got[name]
		if !ok {
			t.Errorf("node %q missing from Ansible inventory", name)
			continue
		}
		if hv["ansible_host"] != ip {
			t.Errorf("node %q: ansible_host = %v, want %v", name, hv["ansible_host"], ip)
		}
		if hv["vm_name"] != name {
			t.Errorf("node %q: vm_name = %v", name, hv["vm_name"])
		}
	}
	for name := range got {
		if _, ok := expected[name]; !ok {
			t.Errorf("inventory contains unknown host %q (not in config.yaml)", name)
		}
	}
}

// TestAnsibleInventoryGroupsAndVIP checks the role groups and the keepalived
// VIP are exported for the provisioning playbook.
func TestAnsibleInventoryGroupsAndVIP(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	data, err := ansible.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	var doc invDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("inventory is not valid YAML: %v", err)
	}

	if doc.All.Vars["lb_vip"] != cfg.Network.Main.VIP {
		t.Errorf("lb_vip = %v, want %v", doc.All.Vars["lb_vip"], cfg.Network.Main.VIP)
	}
	if doc.All.Vars["libvirt_uri"] != cfg.VM.LibvirtURI {
		t.Errorf("libvirt_uri = %v, want %v", doc.All.Vars["libvirt_uri"], cfg.VM.LibvirtURI)
	}

	cases := map[string]int{
		"k3s_servers":    len(cfg.Nodes.Masters),
		"k3s_agents":     len(cfg.Nodes.Workers),
		"load_balancers": len(cfg.Nodes.LBs),
		"cms_frontends":  len(cfg.Nodes.CMSFrontends),
	}
	for grp, want := range cases {
		g, ok := doc.All.Children[grp]
		if !ok {
			t.Errorf("missing group %q", grp)
			continue
		}
		if len(g.Hosts) != want {
			t.Errorf("group %q: %d hosts, want %d", grp, len(g.Hosts), want)
		}
	}

	// The hypervisor play target must exist (libvirt runs there).
	if _, ok := doc.All.Children["hypervisor"]; !ok {
		t.Error("missing 'hypervisor' group")
	}
}

// TestAnsibleInventoryUsesNonRootDeployAccount: the provisioning inventory logs
// in as the configured non-root deploy account with become.
func TestAnsibleInventoryUsesNonRootDeployAccount(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	if got := cfg.SSHUser(); got == "root" {
		t.Fatalf("config.yaml should configure a non-root ssh.user, got %q", got)
	}

	data, err := ansible.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	var doc invDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("inventory is not valid YAML: %v", err)
	}

	vms := doc.All.Children["vms"]
	if vms == nil {
		t.Fatal("inventory has no 'vms' group")
	}
	got := map[string]map[string]any{}
	collectHosts(vms, got)
	if len(got) == 0 {
		t.Fatal("no VM hosts in inventory")
	}
	for name, hv := range got {
		if hv["ansible_user"] != cfg.SSHUser() {
			t.Errorf("node %q: ansible_user = %v, want %v", name, hv["ansible_user"], cfg.SSHUser())
		}
		if hv["ansible_become"] != true {
			t.Errorf("node %q: ansible_become = %v, want true", name, hv["ansible_become"])
		}
	}
}

// TestAnsibleInventoryInterfaces verifies dual-homed nodes get all their NICs
// and single-homed nodes get exactly one, derived from config.yaml MACs.
func TestAnsibleInventoryInterfaces(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatalf("load config.yaml: %v", err)
	}
	inv := ansible.Render(cfg)

	internal := inv.All.Children["vms"].Children["internal"]
	main := inv.All.Children["vms"].Children["main"]

	router, ok := internal.Hosts[cfg.Nodes.Router.Name]
	if !ok {
		t.Fatalf("router %q missing from inventory", cfg.Nodes.Router.Name)
	}
	if len(router.VMInterfaces) != 3 {
		t.Errorf("router should have 3 interfaces (internal/main/WAN), got %d", len(router.VMInterfaces))
	}

	lb1 := main.Hosts[cfg.Nodes.LBs[0].Name]
	if len(lb1.VMInterfaces) != 1 || lb1.VMInterfaces[0].Network != "main" {
		t.Errorf("lb1 interfaces = %+v, want a single 'main' interface", lb1.VMInterfaces)
	}
}
