package cobbler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func cobblerConfig() *config.Config {
	return &config.Config{
		SSH: config.SSHConfig{User: "deploy"},
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{
				CIDR: "10.10.0.0/24", Gateway: "10.10.0.1", Domain: "lab.internal",
			},
			Main: config.NetworkDetail{
				CIDR: "10.20.0.0/24", Gateway: "10.20.0.1", Domain: "lab.main",
			},
		},
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{
				Name:        "jumpstart",
				IP:          "10.10.0.10",
				IPMain:      "10.20.0.10",
				MACInternal: "52:54:00:aa:00:01",
				MACMain:     "52:54:00:aa:00:02",
			},
		},
	}
}

func hasScriptWith(scripts []testutil.Command, substr string) bool {
	for _, s := range scripts {
		if strings.Contains(s.Cmd, substr) {
			return true
		}
	}
	return false
}

func TestInstallCobblerSkipsRebuildButRefreshesTheProfile(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=1 objects=1\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cobblerConfig(), pool: fake}

	if err := p.installCobbler(t.Context(), "10.10.0.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if len(fake.Scripts) < 10 {
		t.Errorf("ran %d scripts, want the full install sequence", len(fake.Scripts))
	}
	if hasScriptWith(fake.Scripts, "rsync -a ") {
		t.Error("current mirror was rebuilt")
	}
	// The template and profile are cheap desired state. They must be refreshed
	// even when the ISO stamp says the mirror is current; otherwise a template
	// change never reaches an existing host.
	if !hasScriptWith(fake.Scripts, "cobbler profile edit") {
		t.Error("steady state should rewrite the profile")
	}
	if !hasScriptWith(fake.Scripts, "cobbler sync") {
		t.Error("steady state should synchronize Cobbler")
	}
}

func TestInstallCobblerRebuildsStaleMirror(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=0 objects=0\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cobblerConfig(), pool: fake}

	if err := p.installCobbler(t.Context(), "10.10.0.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if !hasScriptWith(fake.Scripts, "rsync -a --delete ") {
		t.Error("stale mirror was not rebuilt as an exact copy")
	}
}

func TestInstallCobblerRecreatesObjectsWithoutRebuild(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=1 objects=0\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cobblerConfig(), pool: fake}

	if err := p.installCobbler(t.Context(), "10.10.0.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	if hasScriptWith(fake.Scripts, "rsync -a ") {
		t.Error("fresh mirror was rebuilt even though only the objects were missing")
	}
	if !hasScriptWith(fake.Scripts, "cobbler profile add") {
		t.Error("missing profile was not recreated")
	}
}

func TestInstallCobblerUsesTheConfiguredNetworkAndJumpstartMACs(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, `echo "mirror=$fresh objects=$objects"`) {
				return "mirror=1 objects=1\n", "", 0, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cobblerConfig(), pool: fake}

	if err := p.installCobbler(t.Context(), "10.10.0.10"); err != nil {
		t.Fatalf("installCobbler: %v", err)
	}
	joined := ""
	for _, script := range fake.Scripts {
		joined += script.Cmd + "\n"
	}
	for _, want := range []string{
		"52:54:00:aa:00:01",
		"52:54:00:aa:00:02",
		"10.10.0.0/24(ro,async,no_subtree_check)",
		"10.20.0.0/24(ro,async,no_subtree_check)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("install scripts are missing %q", want)
		}
	}
	for _, unwanted := range []string{
		"52:54:00:10:00:01",
		"52:54:00:10:02:0a",
		"192.168.10.0/24(ro",
		"192.168.20.0/24(ro",
	} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("install scripts still hardcode %q", unwanted)
		}
	}
}

func TestEnsureProfileUsesListForExistenceChecks(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	p := &Phase{pool: fake}

	if err := p.ensureProfile(t.Context(), "10.10.0.10"); err != nil {
		t.Fatalf("ensureProfile: %v", err)
	}
	if len(fake.Scripts) != 1 {
		t.Fatalf("ran %d scripts, want 1", len(fake.Scripts))
	}
	script := fake.Scripts[0].Cmd
	// Cobbler 3.3's `distro report`/`profile report` exit 0 even when the
	// object is missing, so the script must use the list commands instead.
	for _, want := range []string{"cobbler distro list", "cobbler profile list", "grep -Fqx"} {
		if !strings.Contains(script, want) {
			t.Errorf("ensureProfile script does not contain %q", want)
		}
	}
	for _, unwanted := range []string{"cobbler distro report --name", "cobbler profile report --name"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("ensureProfile script uses unreliable existence check %q", unwanted)
		}
	}
}

func TestInstallCobblerStopsAtFailingStep(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "apt-get install -y cobbler") {
				return "", "package not found", 1, nil
			}
			return "", "", 0, nil
		},
	}
	p := &Phase{cfg: cobblerConfig(), pool: fake}

	err := p.installCobbler(t.Context(), "10.10.0.10")
	if err == nil || !strings.Contains(err.Error(), "install step") {
		t.Fatalf("installCobbler() = %v, want a failing install step", err)
	}
	if hasScriptWith(fake.Scripts, "cobbler profile edit") {
		t.Error("phase continued after a failed install step")
	}
}

func TestRenderedTemplatesUseTheConfiguredNetwork(t *testing.T) {
	p := &Phase{cfg: cobblerConfig()}
	rendered, err := p.renderedTemplates()
	if err != nil {
		t.Fatalf("renderedTemplates: %v", err)
	}

	dhcp := string(rendered["/tmp/tpl_dhcp.template"])
	for _, want := range []string{
		"subnet 10.10.0.0 netmask 255.255.255.0",
		"range dynamic-bootp 10.10.0.50 10.10.0.99",
		"next-server 10.10.0.10",
		"option routers 10.10.0.1",
		`option domain-name "lab.internal"`,
		"subnet 10.20.0.0 netmask 255.255.255.0",
		"next-server 10.20.0.10",
	} {
		if !strings.Contains(dhcp, want) {
			t.Errorf("rendered DHCP template is missing %q:\n%s", want, dhcp)
		}
	}

	settings := string(rendered["/tmp/tpl_settings_patch.py"])
	if !strings.Contains(settings, `server = "10.10.0.10"`) {
		t.Errorf("rendered settings-patch.py does not use the jumpstart IP:\n%s", settings)
	}

	autoinstall := string(rendered["/tmp/tpl_autoinstall.yaml"])
	for _, want := range []string{
		"http://10.10.0.10/cblr/pub/authorized_keys",
		"http://10.20.0.10/cblr/pub/authorized_keys",
		"http://10.10.0.10/cblr/svc/op/nopxe/system/$hostname",
		"username: deploy",
		"/target/home/deploy/.ssh/authorized_keys",
		"chown -R deploy:deploy /home/deploy/.ssh",
		"Defaults:deploy !requiretty",
	} {
		if !strings.Contains(autoinstall, want) {
			t.Errorf("rendered autoinstall template is missing %q", want)
		}
	}
	if strings.Contains(autoinstall, "/target/home/ubuntu") {
		t.Errorf("rendered autoinstall template ignores ssh.user:\n%s", autoinstall)
	}

	named := string(rendered["/tmp/tpl_named.template"])
	if strings.Contains(named, "172.20.32.3") {
		t.Errorf("rendered named.template still forwards to the hosted runner resolver:\n%s", named)
	}

	for dst, content := range rendered {
		if strings.Contains(string(content), "{{") {
			t.Errorf("%s still contains an unrendered placeholder:\n%s", dst, content)
		}
	}
}

func TestPlanCobblerNetworkClampsThePoolToTheSubnet(t *testing.T) {
	plan, err := planCobblerNetwork(config.NetworkDetail{CIDR: "10.0.0.0/26", Gateway: "10.0.0.1", Domain: "lab.internal"}, "internal.local")
	if err != nil {
		t.Fatalf("planCobblerNetwork: %v", err)
	}
	if plan.DHCPStart != "10.0.0.50" || plan.DHCPEnd != "10.0.0.62" {
		t.Fatalf("pool = %s-%s, want 10.0.0.50-10.0.0.62", plan.DHCPStart, plan.DHCPEnd)
	}
	if _, err := planCobblerNetwork(config.NetworkDetail{CIDR: "10.0.0.0/27", Gateway: "10.0.0.1"}, "internal.local"); err == nil {
		t.Fatal("expected a /27 to be rejected as too small for the DHCP pool")
	}
}

func TestPlanCobblerNetworkDefaultsOptionalGatewayAndDomain(t *testing.T) {
	plan, err := planCobblerNetwork(config.NetworkDetail{CIDR: "10.30.0.0/24"}, "lab.internal")
	if err != nil {
		t.Fatalf("planCobblerNetwork: %v", err)
	}
	if plan.Gateway != "10.30.0.1" {
		t.Errorf("gateway = %q, want the first host in the subnet", plan.Gateway)
	}
	if plan.Domain != "lab.internal" {
		t.Errorf("domain = %q, want the caller's fallback", plan.Domain)
	}
}

func TestRemoteISOMatchesOnlyOnTheExactSize(t *testing.T) {
	fake := &testutil.FakeExecutor{
		RunFunc: func(_ context.Context, _ string, _ string) (string, string, int, error) {
			return "1234\n", "", 0, nil
		},
	}
	p := &Phase{pool: fake}
	if !p.remoteISOMatches(t.Context(), "10.10.0.10", cobblerISO, 1234) {
		t.Error("remoteISOMatches() = false for the same size")
	}
	if p.remoteISOMatches(t.Context(), "10.10.0.10", cobblerISO, 4321) {
		t.Error("remoteISOMatches() = true for a different size")
	}
}

func TestSelectLocalISOPrefersThePointRelease(t *testing.T) {
	dir := t.TempDir()
	// Sparse files: >1 GB but no disk allocation.
	for _, name := range []string{
		"ubuntu-24.04-live-server-amd64.iso",
		"ubuntu-24.04.4-live-server-amd64.iso",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, 2<<30); err != nil {
			t.Fatal(err)
		}
	}
	got, err := selectLocalISO(dir)
	if err != nil {
		t.Fatalf("selectLocalISO: %v", err)
	}
	if filepath.Base(got) != "ubuntu-24.04.4-live-server-amd64.iso" {
		t.Fatalf("selectLocalISO() = %s, want the 24.04.4 point release", filepath.Base(got))
	}
}
