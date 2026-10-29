package tests

import (
	"os"
	"strings"
	"testing"
)

// TestUFWPuppetChecksMatchTheSingleLinePolicyOutput guards two idempotency
// regressions that only show up as every Puppet run re-applying a default
// policy: ufw prints the three defaults on one line, so only the incoming
// policy is prefixed with "Default:".
func TestUFWPuppetChecksMatchTheSingleLinePolicyOutput(t *testing.T) {
	base := readRepoFile(t, "puppet/modules/role/manifests/base.pp")
	if strings.Contains(base, `grep -q "Default: allow (outgoing)"`) {
		t.Error("base.pp matches a prefixed outgoing policy that ufw never prints")
	}
	if !strings.Contains(base, `grep -q "allow (outgoing)"`) {
		t.Error("base.pp does not check the outgoing policy token")
	}

	router := readRepoFile(t, "puppet/modules/role/manifests/router.pp")
	if strings.Contains(router, `grep -q "Default: deny (routed)"`) {
		t.Error("router.pp matches a prefixed routed policy that ufw never prints")
	}
	if !strings.Contains(router, `grep -q "deny (routed)"`) {
		t.Error("router.pp does not check the routed policy token")
	}
}

// TestNATRulesAreRenderedFromConfiguration keeps the router's DNAT/MASQUERADE
// targets tied to the configured networks. A hardcoded VIP or subnet is a
// silent breakage for any deployment that changes network.*.cidr or
// network.main.vip.
func TestNATRulesAreRenderedFromConfiguration(t *testing.T) {
	nat := readRepoFile(t, "templates/ufw/nat-rules")
	for _, placeholder := range []string{"${WAN_IF}", "${INTERNAL_CIDR}", "${MAIN_CIDR}", "${LB_VIP}"} {
		if !strings.Contains(nat, placeholder) {
			t.Errorf("templates/ufw/nat-rules is missing placeholder %s", placeholder)
		}
	}
	for _, hardcoded := range []string{"192.168.10.0/24", "192.168.20.0/24", "192.168.20.100"} {
		if strings.Contains(nat, hardcoded) {
			t.Errorf("templates/ufw/nat-rules still hardcodes %s", hardcoded)
		}
	}
}

// TestJumpstartUFWScopesProvisioningServices prevents the provisioning server
// from regressing to "allow <port> from anywhere". DHCP is the one exception:
// its clients source from 0.0.0.0, so it is scoped to the two provisioning
// interfaces instead of a source CIDR.
func TestJumpstartUFWScopesProvisioningServices(t *testing.T) {
	jumpstart := readRepoFile(t, "puppet/modules/role/manifests/jumpstart.pp")
	for _, line := range strings.Split(jumpstart, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "/usr/sbin/ufw allow ") {
			continue
		}
		if strings.Contains(trimmed, " from ") || strings.Contains(trimmed, " in on ") {
			continue
		}
		t.Errorf("jumpstart.pp still opens a provisioning port to any source: %s", trimmed)
	}
	for _, want := range []string{
		"$provisioning_sources = [$internal_cidr, $main_cidr]",
		"ufw-remove-public-provisioning-rules",
		"ufw allow in on enp1s0 to any port 67 proto udp",
		"ufw allow in on enp2s0 to any port 67 proto udp",
	} {
		if !strings.Contains(jumpstart, want) {
			t.Errorf("jumpstart.pp is missing %q", want)
		}
	}
}

func TestStorageExportSquashesRoot(t *testing.T) {
	storage := readRepoFile(t, "puppet/modules/role/manifests/storage.pp")
	if strings.Contains(storage, "no_root_squash") {
		t.Error("storage.pp still exports WordPress uploads with no_root_squash")
	}
	if !strings.Contains(storage, "all_squash,anonuid=33,anongid=33") {
		t.Error("storage.pp does not map NFS clients to www-data")
	}
}

func TestWordPressDeployPreservesUploadsAndConfig(t *testing.T) {
	cms := readRepoFile(t, "puppet/modules/role/manifests/cms_frontend.pp")
	if strings.Contains(cms, "rm -rf /var/www/html/*") {
		t.Error("cms_frontend.pp still deletes the whole WordPress tree before deploying")
	}
	if !strings.Contains(cms, "/bin/cp -a /tmp/wordpress/. /var/www/html/") {
		t.Error("cms_frontend.pp no longer deploys WordPress by copying over the existing tree")
	}
}

// TestWordPressDefaultIndexRemovalIsIndependentOfDeployGuard guards the upgrade
// path: wordpress-deploy is guarded by creates, so removing Apache's default
// index.html inside that command only works on a fresh node. On an existing
// node the whole exec is skipped and the "It works" page keeps shadowing
// WordPress. The removal must be its own idempotent resource that still runs
// after the apache2 package may have (re)created the file.
func TestWordPressDefaultIndexRemovalIsIndependentOfDeployGuard(t *testing.T) {
	cms := readRepoFile(t, "puppet/modules/role/manifests/cms_frontend.pp")

	deploy := puppetResourceBlock(t, cms, "exec { 'wordpress-deploy':")
	if strings.Contains(deploy, "/var/www/html/index.html") {
		t.Error("wordpress-deploy still removes index.html behind its creates guard; an upgrade re-run would skip it")
	}

	removal := puppetResourceBlock(t, cms, "file { '/var/www/html/index.html':")
	if !strings.Contains(removal, "ensure  => absent") && !strings.Contains(removal, "ensure => absent") {
		t.Error("index.html removal is not an idempotent ensure => absent file resource")
	}
	if !strings.Contains(removal, "Package['apache2']") {
		t.Error("index.html removal does not run after the apache2 package can recreate it")
	}
}

func puppetResourceBlock(t *testing.T, manifest, header string) string {
	t.Helper()
	start := strings.Index(manifest, header)
	if start < 0 {
		t.Fatalf("manifest does not declare %s", header)
	}
	depth := 0
	for i := start; i < len(manifest); i++ {
		switch manifest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return manifest[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces after %s", header)
	return ""
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
