package routerbootstrap

import (
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"gopkg.in/yaml.v3"
)

// The router netplan is rendered by string replacement, so an unused or
// misspelled placeholder would only surface as an invalid netplan on a live
// router — where a bad file can lock the host out of its own management
// network. Rendering the real template here catches that before a run.
func TestRouterNetplanTemplateRendersValidYAML(t *testing.T) {
	tmpl, err := cms.TemplatesFS.ReadFile("templates/ufw/netplan-router.yaml")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	rendered := strings.NewReplacer(
		"${INT_MAC}", "52:54:00:10:01:02",
		"${MAIN_MAC}", "52:54:00:10:02:02",
		"${WAN_MAC}", "52:54:00:10:00:02",
		"${INT_ADDR}", "192.168.10.1/24",
		"${MAIN_ADDR}", "192.168.20.1/24",
	).Replace(string(tmpl))

	if strings.Contains(rendered, "${") {
		t.Errorf("unrendered placeholder left in the router netplan:\n%s", rendered)
	}

	var doc struct {
		Network struct {
			Version   int                       `yaml:"version"`
			Ethernets map[string]map[string]any `yaml:"ethernets"`
		} `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered netplan is not valid YAML: %v\n%s", err, rendered)
	}
	if doc.Network.Version != 2 {
		t.Errorf("expected netplan version 2, got %d", doc.Network.Version)
	}
	if len(doc.Network.Ethernets) != 3 {
		t.Fatalf("expected 3 ethernet definitions, got %d", len(doc.Network.Ethernets))
	}

	// Both gateway addresses have to be static, and the WAN has to keep DHCP
	// with a metric that beats the default one.
	for name, want := range map[string]string{
		"net0": "192.168.10.1/24",
		"net1": "192.168.20.1/24",
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

	wan, ok := doc.Network.Ethernets["net2"]
	if !ok {
		t.Fatal("missing ethernet net2 (the WAN uplink)")
	}
	if wan["dhcp4"] != true {
		t.Errorf("net2: expected dhcp4=true, got %v", wan["dhcp4"])
	}
	overrides, _ := wan["dhcp4-overrides"].(map[string]any)
	if overrides["route-metric"] != 50 {
		t.Errorf("net2: expected a route metric of 50, got %v", overrides["route-metric"])
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
