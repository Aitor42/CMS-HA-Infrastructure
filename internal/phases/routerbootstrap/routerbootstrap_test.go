package routerbootstrap

import (
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"gopkg.in/yaml.v3"
)

// renderRouterNetplan mirrors what ensureAddresses does, so the template's
// placeholders are exercised with realistic values.
func renderRouterNetplan(t *testing.T) string {
	t.Helper()
	tmpl, err := cms.TemplatesFS.ReadFile("templates/ufw/netplan-router.yaml")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	return strings.NewReplacer(
		"${INT_NAME}", "enp1s0",
		"${MAIN_NAME}", "enp2s0",
		"${WAN_NAME}", "enp3s0",
		"${INT_MAC}", "52:54:00:10:01:02",
		"${MAIN_MAC}", "52:54:00:10:02:02",
		"${WAN_MAC}", "52:54:00:10:00:02",
		"${INT_ADDR}", "192.168.10.1/24",
		"${MAIN_ADDR}", "192.168.20.1/24",
	).Replace(string(tmpl))
}

// The router netplan is rendered by string replacement, so an unused or
// misspelled placeholder would only surface as an invalid netplan on a live
// router — where a bad overlay can lock the host out of its own management
// address. Rendering the real template here catches that before a run.
func TestRouterNetplanTemplateRendersValidYAML(t *testing.T) {
	rendered := renderRouterNetplan(t)

	if strings.Contains(rendered, "${") {
		t.Errorf("unrendered placeholder left in the router netplan:\n%s", rendered)
	}

	var doc struct {
		Network struct {
			Version   int                       `yaml:"version"`
			Renderer  string                    `yaml:"renderer"`
			Ethernets map[string]map[string]any `yaml:"ethernets"`
		} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered netplan is not valid YAML: %v\n%s", err, rendered)
	}
	if doc.Network.Version != 2 {
		t.Errorf("expected netplan version 2, got %d", doc.Network.Version)
	}
	if doc.Network.Renderer != "networkd" {
		t.Errorf("expected the networkd renderer, got %q", doc.Network.Renderer)
	}
	if len(doc.Network.Ethernets) != 3 {
		t.Fatalf("expected 3 ethernet definitions, got %d", len(doc.Network.Ethernets))
	}

	// Both gateway addresses have to be static (the client subnets point their
	// DHCP gateway at exactly these), keyed on the interface names the phase
	// discovered on the router.
	for name, want := range map[string]string{
		"enp1s0": "192.168.10.1/24",
		"enp2s0": "192.168.20.1/24",
	} {
		def, ok := doc.Network.Ethernets[name]
		if !ok {
			t.Fatalf("missing ethernet %q", name)
		}
		if def["dhcp4"] != false {
			t.Errorf("%s: expected dhcp4=false, got %v", name, def["dhcp4"])
		}
		addrs, _ := def["addresses"].([]any)
		if len(addrs) != 1 || addrs[0] != want {
			t.Errorf("%s: expected address %s, got %v", name, want, def["addresses"])
		}
	}

	// The WAN keeps DHCP but has to win the default route, and it is the
	// router's only source of DNS once the installer's definition is retired.
	wan, ok := doc.Network.Ethernets["enp3s0"]
	if !ok {
		t.Fatal("missing ethernet enp3s0 (the WAN uplink)")
	}
	if wan["dhcp4"] != true {
		t.Errorf("enp3s0: expected dhcp4=true, got %v", wan["dhcp4"])
	}
	overrides, _ := wan["dhcp4-overrides"].(map[string]any)
	if overrides["route-metric"] != 50 {
		t.Errorf("enp3s0: expected a route metric of 50, got %v", overrides["route-metric"])
	}
	if overrides["use-dns"] != true {
		t.Errorf("enp3s0: expected use-dns=true, got %v", overrides["use-dns"])
	}
}

// The two remote scripts are built with fmt.Sprintf, so a stray verb or an
// unescaped percent sign would end up on a live router. Rendering them here
// also keeps the rollback safety net covered: a broken overlay has to restore
// the installer's configuration rather than lock the router out.
func TestNetplanScriptsRender(t *testing.T) {
	bootstrap := netplanBootstrapScript("/tmp/staged.yaml", "/root/cms-netplan-backup", "/etc/netplan/99-cms-router.yaml")
	restore := netplanRestoreScript("/root/cms-netplan-backup", "/etc/netplan/99-cms-router.yaml")

	for name, script := range map[string]string{"bootstrap": bootstrap, "restore": restore} {
		if strings.Contains(script, "%!") {
			t.Errorf("%s script has a bad Sprintf verb:\n%s", name, script)
		}
		if strings.Contains(script, "%%") || strings.Contains(script, "%[") {
			t.Errorf("%s script has an unrendered placeholder:\n%s", name, script)
		}
	}

	for _, want := range []string{
		"install -m 600 -o root -g root /tmp/staged.yaml \"$TARGET\"",
		"BACKUP=/root/cms-netplan-backup",
		"TARGET=/etc/netplan/99-cms-router.yaml",
		"-printf '%f '",
		"netplan generate",
		"netplan apply",
		"CMS_NETPLAN_APPLY_STARTED",
		"$BACKUP/.cms-backup-complete",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap script is missing %q:\n%s", want, bootstrap)
		}
	}
	for _, want := range []string{
		"rm -f /etc/netplan/99-cms-router.yaml",
		"cp -a /root/cms-netplan-backup/. /etc/netplan/",
		"CMS_NETPLAN_RESTORED",
	} {
		if !strings.Contains(restore, want) {
			t.Errorf("restore script is missing %q:\n%s", want, restore)
		}
	}
}

func TestWithPrefix(t *testing.T) {
	cases := []struct {
		ip, cidr, want string
	}{
		{"192.168.10.1", "192.168.10.0/24", "192.168.10.1/24"},
		{"192.168.20.1", "192.168.20.0/16", "192.168.20.1/16"},
		{"192.168.20.1", "192.168.20.0", "192.168.20.1/24"},
		{"", "192.168.20.0/24", ""},
	}
	for _, c := range cases {
		if got := withPrefix(c.ip, c.cidr, "24"); got != c.want {
			t.Errorf("withPrefix(%q, %q) = %q, want %q", c.ip, c.cidr, got, c.want)
		}
	}
}
