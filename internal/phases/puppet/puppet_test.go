package puppet

import (
	"context"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func TestCatalogConcurrencyResolution(t *testing.T) {
	t.Setenv("PUPPET_CONCURRENCY", "")
	p := &Phase{cfg: &config.Config{Deploy: config.DeployConfig{PuppetConcurrency: 6}}}
	if got, source := p.catalogConcurrency(); got != 6 || source != "config" {
		t.Fatalf("config concurrency = (%d, %q), want (6, config)", got, source)
	}

	t.Setenv("PUPPET_CONCURRENCY", "7")
	if got, source := p.catalogConcurrency(); got != 7 || source != "PUPPET_CONCURRENCY" {
		t.Fatalf("env concurrency = (%d, %q), want (7, PUPPET_CONCURRENCY)", got, source)
	}

	// An invalid override falls through to config/auto instead of failing.
	t.Setenv("PUPPET_CONCURRENCY", "many")
	if got, source := p.catalogConcurrency(); got != 6 || source != "config" {
		t.Fatalf("invalid env concurrency = (%d, %q), want (6, config)", got, source)
	}

	p.cfg.Deploy.PuppetConcurrency = 0
	got, source := p.catalogConcurrency()
	if source != "auto" {
		t.Fatalf("auto source = %q, want auto", source)
	}
	if got < 4 || got > 8 {
		t.Fatalf("auto concurrency = %d, want 4..8", got)
	}
}

func TestAutosignPolicyIsExact(t *testing.T) {
	policy := autosignPolicy("jumpstart.internal.local", []config.NodeSpec{
		{FQDN: "internal-master1.internal.local"},
		{FQDN: "main-lb1.main.local"},
		{FQDN: "internal-master1.internal.local"}, // duplicate
		{FQDN: ""}, // skipped
	})
	if strings.Contains(policy, "*") {
		t.Fatalf("autosign policy must not contain a wildcard: %q", policy)
	}
	want := "internal-master1.internal.local\njumpstart.internal.local\nmain-lb1.main.local\n"
	if policy != want {
		t.Fatalf("autosign policy = %q, want %q", policy, want)
	}
}

func TestJumpstartServerNameUsesConfiguredFQDN(t *testing.T) {
	p := &Phase{cfg: &config.Config{Nodes: config.NodesConfig{Jumpstart: config.NodeDetail{FQDN: "puppet.lab.example"}}}}
	if got := p.jumpstartServerName(); got != "puppet.lab.example" {
		t.Fatalf("jumpstartServerName() = %q, want configured FQDN", got)
	}
	p.cfg.Nodes.Jumpstart.FQDN = "  "
	if got := p.jumpstartServerName(); got != defaultJumpstartFQDN {
		t.Fatalf("jumpstartServerName() = %q, want fallback %q", got, defaultJumpstartFQDN)
	}
}

func TestFirstCatalogCommandUsesConfiguredServerAndKeepsDaemonRunning(t *testing.T) {
	cmd := firstCatalogCommand("puppet.lab.example")
	for _, want := range []string{
		"--server 'puppet.lab.example'",
		"systemctl stop puppet",
		"systemctl start puppet",
		"systemctl is-active --quiet puppet",
		"exit $code",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("firstCatalogCommand() is missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "jumpstart.internal.local") {
		t.Error("firstCatalogCommand() still hardcodes the default jumpstart name")
	}
}

func TestIsPuppetSSLResetError(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   bool
	}{
		{"private key mismatch", "", "The certificate does not match its private key", true},
		{"signature failure", "", "Error: certificate signature failure", true},
		{"verify failure", "SSL_connect returned=1 errno=0 state=error: certificate verify failed", "", true},
		{"ordinary compile error", "", "Error: Could not find class role::missing", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPuppetSSLResetError(tc.stdout, tc.stderr); got != tc.want {
				t.Fatalf("isPuppetSSLResetError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDatabaseHostsUsesConfiguredMastersAndPort(t *testing.T) {
	p := &Phase{cfg: &config.Config{
		Database: config.DatabaseConfig{Port: 3307},
		Nodes: config.NodesConfig{Masters: []config.NodeDetail{
			{IP: "10.0.0.11"},
			{IP: "10.0.0.12"},
			{IP: "10.0.0.13"},
		}},
	}}
	if got, want := p.databaseHosts(), "10.0.0.11:3307,10.0.0.12:3307"; got != want {
		t.Fatalf("databaseHosts() = %q, want %q", got, want)
	}
	p.cfg.Database.Port = 0
	if got, want := p.databaseHosts(), "10.0.0.11:30306,10.0.0.12:30306"; got != want {
		t.Fatalf("databaseHosts() default port = %q, want %q", got, want)
	}
}

func TestUploadPuppetCodeClearsStaleCodeAndChecksPermissions(t *testing.T) {
	t.Chdir("../../..")
	fake := &testutil.FakeExecutor{}
	p := &Phase{pool: fake}
	if err := p.uploadPuppetCode(context.Background(), "192.168.10.10"); err != nil {
		t.Fatalf("uploadPuppetCode() = %v", err)
	}
	for _, want := range []string{
		"mkdir -p /etc/puppetlabs/code/environments/production",
		"find /etc/puppetlabs/code/environments/production -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +",
		"chown -R puppet:puppet",
		"test -f /etc/puppetlabs/code/environments/production/manifests/site.pp",
	} {
		found := false
		for _, command := range fake.Commands {
			if strings.Contains(command.Cmd, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("uploadPuppetCode() did not run %q", want)
		}
	}
}

func TestUploadPuppetCodeFailsWhenRemoteSetupFails(t *testing.T) {
	t.Chdir("../../..")
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{"mkdir", "mkdir -p", "create remote code directory"},
		{"clear", "find /etc/puppetlabs", "clear remote code directory"},
		{"chown", "chown -R puppet:puppet", "secure/verify remote code"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
				if strings.Contains(cmd, tc.cmd) {
					return "", "failed", 1, nil
				}
				return "", "", 0, nil
			}}
			p := &Phase{pool: fake}
			if err := p.uploadPuppetCode(context.Background(), "192.168.10.10"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("uploadPuppetCode() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
