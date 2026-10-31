package registernodes

import (
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func registerConfig() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "10.10.0.0/24"},
			Main:     config.NetworkDetail{CIDR: "10.20.0.0/24"},
		},
		Nodes: config.NodesConfig{
			Jumpstart: config.NodeDetail{
				Name: "jumpstart", IP: "10.10.0.10", IPMain: "10.20.0.10",
				MACInternal: "52:54:00:aa:00:01", MACMain: "52:54:00:aa:00:02",
			},
			Masters: []config.NodeDetail{
				{Name: "internal-master1", IP: "10.10.0.11", FQDN: "internal-master1.lab.internal", MAC: "52:54:00:aa:00:11"},
			},
			LBs: []config.NodeDetail{
				{Name: "main-lb1", IP: "10.20.0.103", FQDN: "main-lb1.lab.main", MACMain: "52:54:00:aa:01:03"},
			},
		},
	}
}

func TestRegistrationScriptUsesTheCobblerProfileAndBothServerAddresses(t *testing.T) {
	p := &Phase{cfg: registerConfig()}
	script, err := p.registrationScript()
	if err != nil {
		t.Fatalf("registrationScript: %v", err)
	}

	for _, want := range []string{
		"--profile='ubuntu-24.04-x86_64'",
		"--name='internal-master1'",
		"--server='10.10.0.10'",
		"--name='main-lb1'",
		"--server='10.20.0.10'",
		"nfsroot=10.20.0.10:/var/www/cobbler/distro_mirror/ubuntu-24.04",
		"--autoinstall-meta='hostname=main-lb1'",
		"cobbler sync",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("registration script is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "--name='jumpstart'") {
		t.Error("the jumpstart must not be registered as a client")
	}
}

func TestRegistrationScriptRejectsAMainNodeWithoutTheJumpstartMainIP(t *testing.T) {
	cfg := registerConfig()
	cfg.Nodes.Jumpstart.IPMain = ""
	p := &Phase{cfg: cfg}

	if _, err := p.registrationScript(); err == nil || !strings.Contains(err.Error(), "ip_main") {
		t.Fatalf("registrationScript() = %v, want the missing ip_main error", err)
	}
}

func TestRegistrationScriptRejectsNodesWithoutMACOrFQDN(t *testing.T) {
	t.Run("missing MAC", func(t *testing.T) {
		cfg := registerConfig()
		cfg.Nodes.Masters[0].MAC = ""
		p := &Phase{cfg: cfg}
		if _, err := p.registrationScript(); err == nil || !strings.Contains(err.Error(), "MAC") {
			t.Fatalf("registrationScript() = %v, want the missing MAC error", err)
		}
	})
	t.Run("missing FQDN", func(t *testing.T) {
		cfg := registerConfig()
		cfg.Nodes.Masters[0].FQDN = ""
		p := &Phase{cfg: cfg}
		if _, err := p.registrationScript(); err == nil || !strings.Contains(err.Error(), "FQDN") {
			t.Fatalf("registrationScript() = %v, want the missing FQDN error", err)
		}
	})
}

func TestRunRejectsAnEmptyJumpstartIPBeforeTouchingCobbler(t *testing.T) {
	cfg := registerConfig()
	cfg.Nodes.Jumpstart.IP = ""
	fake := &testutil.FakeExecutor{}
	p := &Phase{cfg: cfg, pool: fake}

	if err := p.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "jumpstart.ip") {
		t.Fatalf("Run() = %v, want the empty jumpstart IP error", err)
	}
	if len(fake.Scripts) != 0 {
		t.Errorf("Run() executed %d scripts before validating the jumpstart IP", len(fake.Scripts))
	}
}

func TestRunExecutesTheRegistrationScriptOnTheJumpstart(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	p := &Phase{cfg: registerConfig(), pool: fake}

	if err := p.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.Scripts) != 1 {
		t.Fatalf("Run() executed %d scripts, want 1", len(fake.Scripts))
	}
	if fake.Scripts[0].Host != "10.10.0.10" {
		t.Errorf("registration script ran on %s, want the jumpstart", fake.Scripts[0].Host)
	}
	if !strings.Contains(fake.Scripts[0].Cmd, "cobbler sync") {
		t.Error("registration script did not synchronize Cobbler")
	}
}
