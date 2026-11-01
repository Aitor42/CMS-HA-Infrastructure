package puppet

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/config"
	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
	"gopkg.in/yaml.v3"
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
		{Name: "no-fqdn"},                         // falls back to the short name
		{FQDN: ""},                                // skipped: no FQDN and no name
	})
	if strings.Contains(policy, "*") {
		t.Fatalf("autosign policy must not contain a wildcard: %q", policy)
	}
	want := "internal-master1.internal.local\njumpstart.internal.local\nmain-lb1.main.local\nno-fqdn\n"
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

func puppetTestConfig() *config.Config {
	return &config.Config{
		SSH: config.SSHConfig{User: "deploy"},
		PKI: config.PKIConfig{Domain: "ca.lab.internal"},
		Network: config.NetworkConfig{
			Internal: config.NetworkDetail{CIDR: "10.10.0.0/24", Domain: "lab.internal"},
			Main:     config.NetworkDetail{CIDR: "10.20.0.0/24", Domain: "lab.main", VIP: "10.20.0.100"},
		},
		Nodes: config.NodesConfig{
			Router:    config.NodeDetail{Name: "gw1", IP: "10.10.0.1", FQDN: "gw1.lab.internal"},
			Jumpstart: config.NodeDetail{Name: "provisioner", IP: "10.10.0.10", IPMain: "10.20.0.10", FQDN: "provisioner.lab.internal"},
			Masters: []config.NodeDetail{
				{Name: "cp1", IP: "10.10.0.11", FQDN: "cp1.lab.internal"},
				{Name: "cp2", IP: "10.10.0.12", FQDN: "cp2.lab.internal"},
				{Name: "cp3", IP: "10.10.0.13", FQDN: "cp3.lab.internal"},
			},
			Workers: []config.NodeDetail{{Name: "wk1", IP: "10.10.0.14", FQDN: "wk1.lab.internal"}},
			Storage: config.NodeDetail{Name: "store1", IP: "10.10.0.15", FQDN: "store1.lab.internal"},
			Monitor: config.NodeDetail{Name: "obs1", IP: "10.10.0.16", FQDN: "obs1.lab.internal"},
			LBs: []config.NodeDetail{
				{Name: "edge1", IP: "10.20.0.103", FQDN: "edge1.lab.main"},
				{Name: "edge2", IP: "10.20.0.104", FQDN: "edge2.lab.main"},
			},
			CMSFrontends: []config.NodeDetail{{Name: "web1", IP: "10.20.0.101", FQDN: "web1.lab.main"}},
			Hotdesks:     config.HotdesksConf{Count: 1, BaseIPOctet: 201},
		},
	}
}

func TestGeneratedSitePPClassifiesConfiguredNodesByName(t *testing.T) {
	p := &Phase{cfg: puppetTestConfig()}
	out, err := p.generatedSitePP()
	if err != nil {
		t.Fatalf("generatedSitePP: %v", err)
	}
	site := string(out)
	for _, want := range []string{
		"node 'provisioner.lab.internal'",
		"include role::jumpstart",
		"node 'gw1.lab.internal'",
		"include role::router",
		"node 'cp1.lab.internal'",
		"include role::k3s_master",
		"node 'wk1.lab.internal'",
		"include role::k3s_worker",
		"node 'store1.lab.internal'",
		"include role::storage",
		"node 'obs1.lab.internal'",
		"include role::monitor",
		"node 'web1.lab.main'",
		"include role::cms_frontend",
		"node 'main-hotdesk1.lab.main'",
		"include role::hotdesk",
		"node default",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("generated site.pp is missing %q:\n%s", want, site)
		}
	}
	// The committed classifier matches hostname regexes; the generated one must
	// not depend on the default names.
	for _, unwanted := range []string{"node /^", "ufw-router", "internal-master", "main.local"} {
		if strings.Contains(site, unwanted) {
			t.Errorf("generated site.pp still contains %q:\n%s", unwanted, site)
		}
	}

	// The first configured LB is the keepalived master regardless of its name.
	first := strings.Index(site, "node 'edge1.lab.main'")
	second := strings.Index(site, "node 'edge2.lab.main'")
	if first < 0 || second < 0 {
		t.Fatalf("generated site.pp is missing a load balancer:\n%s", site)
	}
	firstBlock := site[first:second]
	secondBlock := site[second:]
	// is_master must be a Boolean literal: role::loadbalancer types it as
	// Boolean, and a quoted 'true' is a String that fails the catalog.
	for _, want := range []string{"is_master => true,", "master_ip => '10.20.0.103'", "backup_ip => '10.20.0.104'", "vip => '10.20.0.100'"} {
		if !strings.Contains(firstBlock, want) {
			t.Errorf("first LB block is missing %q:\n%s", want, firstBlock)
		}
	}
	if !strings.Contains(secondBlock, "is_master => false,") {
		t.Errorf("second LB is not the backup:\n%s", secondBlock)
	}
	if strings.Contains(site, "is_master => '") {
		t.Errorf("generated site.pp quotes is_master, which Puppet rejects for a Boolean parameter:\n%s", site)
	}
}

func TestGeneratedHieraDataUsesTheConfiguredTopology(t *testing.T) {
	p := &Phase{cfg: puppetTestConfig()}
	out, err := p.generatedHieraData()
	if err != nil {
		t.Fatalf("generatedHieraData: %v", err)
	}
	var data map[string]string
	if err := yaml.Unmarshal(out, &data); err != nil {
		t.Fatalf("generated Hiera data is not valid YAML: %v\n%s", err, out)
	}
	for key, want := range map[string]string{
		"cms_ha::deploy_user":       "deploy",
		"cms_ha::jumpstart_ip":      "10.10.0.10",
		"cms_ha::jumpstart_ip_main": "10.20.0.10",
		"cms_ha::internal_cidr":     "10.10.0.0/24",
		"cms_ha::main_cidr":         "10.20.0.0/24",
		"cms_ha::main_vip":          "10.20.0.100",
		"cms_ha::monitor_ip":        "10.10.0.16",
		"cms_ha::storage_ip":        "10.10.0.15",
	} {
		if data[key] != want {
			t.Errorf("%s = %q, want %q", key, data[key], want)
		}
	}
	hosts := data["cms_ha::hosts_content"]
	for _, want := range []string{
		"10.10.0.1\tgw1.lab.internal gw1",
		"10.10.0.10\tprovisioner.lab.internal provisioner puppet ca.lab.internal ca",
		"10.20.0.100\tmain-lb.lab.main main-lb",
	} {
		if !strings.Contains(hosts, want) {
			t.Errorf("generated hosts content is missing %q:\n%s", want, hosts)
		}
	}
}

func TestManifestsOnlyLookUpHieraKeysThePhaseGenerates(t *testing.T) {
	// A manifest that looks up a key the phase never writes would fail the
	// catalog on the first agent run (or, with a default, silently keep the
	// default lab). Keep the lookup set and the generated data in lockstep.
	p := &Phase{cfg: puppetTestConfig()}
	raw, err := p.generatedHieraData()
	if err != nil {
		t.Fatalf("generatedHieraData: %v", err)
	}
	var generated map[string]string
	if err := yaml.Unmarshal(raw, &generated); err != nil {
		t.Fatalf("generated Hiera data: %v", err)
	}

	pattern := regexp.MustCompile(`lookup\('(cms_ha::[a-z0-9_]+)'`)
	manifests, err := filepath.Glob(filepath.Join("..", "..", "..", "puppet", "modules", "role", "manifests", "*.pp"))
	if err != nil {
		t.Fatalf("glob manifests: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no Puppet manifests found")
	}
	seen := 0
	for _, path := range manifests {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			seen++
			if _, ok := generated[match[1]]; !ok {
				t.Errorf("%s looks up %s, which generatedHieraData does not write", filepath.Base(path), match[1])
			}
		}
	}
	if seen == 0 {
		t.Fatal("no Hiera lookups found in the role manifests")
	}
}

func TestUploadPuppetCodeClearsStaleCodeAndChecksPermissions(t *testing.T) {
	t.Chdir("../../..")
	fake := &testutil.FakeExecutor{}
	p := &Phase{cfg: puppetTestConfig(), pool: fake}
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
			p := &Phase{cfg: puppetTestConfig(), pool: fake}
			if err := p.uploadPuppetCode(context.Background(), "192.168.10.10"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("uploadPuppetCode() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
