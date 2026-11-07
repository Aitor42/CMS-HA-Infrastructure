package routerbootstrap

import (
	"context"
	"strings"
	"testing"

	cms "github.com/Aitor42/CMS-HA-Infrastructure"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
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

func TestRouterGatewayAddressesComeFromTheNetworkConfig(t *testing.T) {
	cfg := &config.Config{
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "10.10.0.0/24", Gateway: "10.10.0.1"},
			Main:     config.NetworkDetail{CIDR: "10.20.0.0/16", Gateway: "10.20.0.1"},
		},
		// Even a mismatched node address must not leak into the netplan: the
		// clients route through the gateway, not through nodes.router.ip.
		Nodes: config.NodesConfig{Router: config.NodeDetail{IP: "10.10.0.99"}},
	}
	intAddr, mainAddr, err := routerGatewayAddresses(cfg)
	if err != nil {
		t.Fatalf("routerGatewayAddresses: %v", err)
	}
	if intAddr != "10.10.0.1/24" || mainAddr != "10.20.0.1/16" {
		t.Fatalf("addresses = %s / %s, want the configured gateways", intAddr, mainAddr)
	}

	cfg.Network.Main.Gateway = ""
	if _, _, err := routerGatewayAddresses(cfg); err == nil || !strings.Contains(err.Error(), "main-network") {
		t.Fatalf("routerGatewayAddresses() = %v, want the missing main gateway error", err)
	}
}

func TestIfaceByMACRejectsAmbiguousOrInvalidResults(t *testing.T) {
	cases := []struct {
		name    string
		mac     string
		output  string
		wantErr string
	}{
		{"invalid MAC", "not-a-mac", "eth0", "invalid MAC"},
		{"no match", "52:54:00:10:00:02", "", "no interface"},
		{"two matches", "52:54:00:10:00:02", "eth0\neth1\n", "multiple interfaces"},
		{"invalid name", "52:54:00:10:00:02", "eth0!", "invalid interface name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &testutil.FakeExecutor{
				RunFunc: func(_ context.Context, _, _ string) (string, string, int, error) {
					return tc.output, "", 0, nil
				},
			}
			p := &Phase{pool: fake}
			if _, err := p.ifaceByMAC(t.Context(), "192.168.10.1", tc.mac); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ifaceByMAC() = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestProbeFromDoesNotLetWgetRetryABlackholedRoute(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _, _ string) (string, string, int, error) {
			return "CMS_PROBE_OK\n", "", 0, nil
		},
	}
	p := &Phase{pool: fake}
	if err := p.probeFrom(t.Context(), "192.168.10.11", "master1"); err != nil {
		t.Fatalf("probeFrom: %v", err)
	}
	if len(fake.Commands) != 1 {
		t.Fatalf("ran %d commands, want 1", len(fake.Commands))
	}
	if !strings.Contains(fake.Commands[0].Cmd, "--tries=1") {
		t.Errorf("probe command lets wget retry a blackholed route:\n%s", fake.Commands[0].Cmd)
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
